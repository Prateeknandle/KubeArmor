// SPDX-License-Identifier: Apache-2.0
// Copyright 2021 Authors of KubeArmor

// Package protectenv contains components for protectenv preset rule
package protectenv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/kubearmor/KubeArmor/KubeArmor/presets/base"

	fd "github.com/kubearmor/KubeArmor/KubeArmor/feeder"
	mon "github.com/kubearmor/KubeArmor/KubeArmor/monitor"
	tp "github.com/kubearmor/KubeArmor/KubeArmor/types"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang protectenv  ../../BPF/protectenv.bpf.c -type pevent -no-global-types -- -I/usr/include/ -O2 -g

type eventBPF struct {
	Pid   uint32
	PidNS uint32
	MntNS uint32
	Comm  [80]uint8
}

// EnvPreset struct
type EnvPreset struct {
	base.Preset

	BPFContainerMap *ebpf.Map

	// events
	Events        *ringbuf.Reader
	EventsChannel chan []byte

	// ContainerID -> NsKey + rules
	ContainerMap     map[string]interface{}
	ContainerMapLock *sync.RWMutex

	Link link.Link

	obj protectenvObjects
}

// RegisterPreset register protectenv preset and returns an instance of EnvPreset on success
// otherwise returns an error
func RegisterPreset(logger *fd.Feeder, monitor *mon.SystemMonitor) (*EnvPreset, error) {
	p := &EnvPreset{}

	p.Logger = logger
	p.Monitor = monitor
	var err error

	if err = rlimit.RemoveMemlock(); err != nil {
		p.Logger.Errf("Error removing rlimit %v", err)
		return nil, nil // Doesn't require clean up so not returning err
	}

	p.BPFContainerMap, _ = ebpf.NewMapWithOptions(&ebpf.MapSpec{
		Type:       ebpf.Hash,
		KeySize:    8,
		ValueSize:  4,
		MaxEntries: 256,
		Pinning:    ebpf.PinByName,
		Name:       "kubearmor_preset_containers",
	}, ebpf.MapOptions{})

	if err := loadProtectenvObjects(&p.obj, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{},
	}); err != nil {
		p.Logger.Errf("error loading BPF LSM objects: %v", err)
		return p, err
	}

	p.Link, err = link.AttachLSM(link.LSMOptions{Program: p.obj.EnforceFile})
	if err != nil {
		p.Logger.Errf("opening lsm %s: %s", p.obj.EnforceFile.String(), err)
		return p, err
	}

	p.Events, err = ringbuf.NewReader(p.obj.Events)
	if err != nil {
		p.Logger.Errf("opening ringbuf reader: %s", err)
		return p, err
	}
	p.EventsChannel = make(chan []byte, mon.SyscallChannelSize)

	go p.TraceEvents()

	return p, nil

}

// TraceEvents traces events generated by bpflsm enforcer
func (p *EnvPreset) TraceEvents() {

	if p.Events == nil {
		p.Logger.Err("ringbuf reader is nil, exiting trace events")
	}
	p.Logger.Print("Starting TraceEvents from Env Presets")
	go func() {
		for {

			record, err := p.Events.Read()
			if err != nil {
				if errors.Is(err, ringbuf.ErrClosed) {
					// This should only happen when we call DestroyMonitor while terminating the process.
					// Adding a Warn just in case it happens at runtime, to help debug
					p.Logger.Warnf("Ring Buffer closed, exiting TraceEvents %s", err.Error())
					return
				}
				p.Logger.Warnf("Ringbuf error reading %s", err.Error())
				continue
			}

			p.EventsChannel <- record.RawSample

		}
	}()

	for {

		dataRaw := <-p.EventsChannel

		var event eventBPF

		if err := binary.Read(bytes.NewBuffer(dataRaw), binary.LittleEndian, &event); err != nil {
			log.Printf("parsing ringbuf event: %s", err)
			continue
		}

		containerID := ""

		if event.PidNS != 0 && event.MntNS != 0 {
			containerID = p.Monitor.LookupContainerID(event.PidNS, event.MntNS)
		}

		if containerID != "" {
			p.Logger.Printf("Alert event from cid %s for protect env preset with deets %+v", containerID, event)
		}

	}
}

// RegisterContainer registers a container
func (p *EnvPreset) RegisterContainer(containerID string, pidns, mntns uint32) {

}

// UnregisterContainer unregisters a container
func (p *EnvPreset) UnregisterContainer(containerID string) {

}

// UpdateSecurityPolicies updates protectenv policy rules
func (p *EnvPreset) UpdateSecurityPolicies(endPoint tp.EndPoint) {

}

// Destroy func deletes EnvPreset instance
func (p *EnvPreset) Destroy() error {
	return nil
}
