// Copyright (C) 2014 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package discover

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/syncthing/protocol"
	"github.com/syncthing/syncthing/lib/beacon"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/thejerf/suture"
)

type localClient struct {
	*suture.Supervisor
	myID      protocol.DeviceID
	addrList  AddressLister
	relayStat RelayStatusProvider
	name      string

	beacon          beacon.Interface
	localBcastStart time.Time
	localBcastTick  <-chan time.Time
	forcedBcastTick chan time.Time

	*cache
}

const (
	BroadcastInterval = 30 * time.Second
	CacheLifeTime     = 3 * BroadcastInterval
)

var (
	ErrIncorrectMagic = errors.New("incorrect magic number")
)

func NewLocal(id protocol.DeviceID, addr string, addrList AddressLister, relayStat RelayStatusProvider) (FinderService, error) {
	c := &localClient{
		Supervisor:      suture.NewSimple("local"),
		myID:            id,
		addrList:        addrList,
		relayStat:       relayStat,
		localBcastTick:  time.Tick(BroadcastInterval),
		forcedBcastTick: make(chan time.Time),
		localBcastStart: time.Now(),
		cache:           newCache(),
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	if len(host) == 0 {
		// A broadcast client
		c.name = "IPv4 local"
		bcPort, err := strconv.Atoi(port)
		if err != nil {
			return nil, err
		}
		c.startLocalIPv4Broadcasts(bcPort)
	} else {
		// A multicast client
		c.name = "IPv6 local"
		c.startLocalIPv6Multicasts(addr)
	}

	go c.sendLocalAnnouncements()

	return c, nil
}

func (c *localClient) startLocalIPv4Broadcasts(localPort int) {
	c.beacon = beacon.NewBroadcast(localPort)
	c.Add(c.beacon)
	go c.recvAnnouncements(c.beacon)
}

func (c *localClient) startLocalIPv6Multicasts(localMCAddr string) {
	c.beacon = beacon.NewMulticast(localMCAddr)
	c.Add(c.beacon)
	go c.recvAnnouncements(c.beacon)
}

// Lookup returns a list of addresses the device is available at. Local
// discovery never returns relays.
func (c *localClient) Lookup(device protocol.DeviceID) (direct []string, relays []Relay, err error) {
	if cache, ok := c.Get(device); ok {
		if time.Since(cache.when) < CacheLifeTime {
			direct = cache.Direct
			relays = cache.Relays
		}
	}

	return
}

func (c *localClient) String() string {
	return c.name
}

func (c *localClient) Error() error {
	return c.beacon.Error()
}

func (c *localClient) announcementPkt() Announce {
	addrs := c.addrList.AllAddresses()

	var relays []Relay
	for _, relay := range c.relayStat.Relays() {
		latency, ok := c.relayStat.RelayStatus(relay)
		if ok {
			relays = append(relays, Relay{
				URL:     relay,
				Latency: int32(latency / time.Millisecond),
			})
		}
	}

	return Announce{
		Magic: AnnouncementMagic,
		This: Device{
			ID:        c.myID[:],
			Addresses: addrs,
			Relays:    relays,
		},
	}
}

func (c *localClient) sendLocalAnnouncements() {
	var pkt = c.announcementPkt()
	msg := pkt.MustMarshalXDR()

	for {
		c.beacon.Send(msg)

		select {
		case <-c.localBcastTick:
		case <-c.forcedBcastTick:
		}
	}
}

func (c *localClient) recvAnnouncements(b beacon.Interface) {
	for {
		buf, addr := b.Recv()

		var pkt Announce
		err := pkt.UnmarshalXDR(buf)
		if err != nil && err != io.EOF {
			if debug {
				l.Debugf("discover: Failed to unmarshal local announcement from %s:\n%s", addr, hex.Dump(buf))
			}
			continue
		}

		if debug {
			l.Debugf("discover: Received local announcement from %s for %s", addr, protocol.DeviceIDFromBytes(pkt.This.ID))
		}

		var newDevice bool
		if bytes.Compare(pkt.This.ID, c.myID[:]) != 0 {
			newDevice = c.registerDevice(addr, pkt.This)
		}

		if newDevice {
			select {
			case c.forcedBcastTick <- time.Now():
			}
		}
	}
}

func (c *localClient) registerDevice(src net.Addr, device Device) bool {
	var id protocol.DeviceID
	copy(id[:], device.ID)

	// Remember whether we already had a valid cache entry for this device.

	ce, existsAlready := c.Get(id)
	isNewDevice := !existsAlready || time.Since(ce.when) > CacheLifeTime

	// Any empty or unspecified addresses should be set to the source address
	// of the announcement. We also skip any addresses we can't parse.

	var validAddresses []string
	for _, addr := range device.Addresses {
		u, err := url.Parse(addr)
		if err != nil {
			continue
		}

		tcpAddr, err := net.ResolveTCPAddr("tcp", u.Host)
		if err != nil {
			continue
		}

		if len(tcpAddr.IP) == 0 || tcpAddr.IP.IsUnspecified() {
			host, _, err := net.SplitHostPort(src.String())
			if err != nil {
				continue
			}
			u.Host = fmt.Sprintf("%s:%d", host, tcpAddr.Port)
			validAddresses = append(validAddresses, u.String())
		} else {
			validAddresses = append(validAddresses, addr)
		}
	}

	c.Set(id, CacheEntry{
		Direct: validAddresses,
		Relays: device.Relays,
		when:   time.Now(),
		found:  true,
	})

	if isNewDevice {
		events.Default.Log(events.DeviceDiscovered, map[string]interface{}{
			"device": id.String(),
			"addrs":  device.Addresses,
			"relays": device.Relays,
		})
	}

	return isNewDevice
}

func addrToAddr(addr *net.TCPAddr) string {
	if len(addr.IP) == 0 || addr.IP.IsUnspecified() {
		return fmt.Sprintf(":%c", addr.Port)
	} else if bs := addr.IP.To4(); bs != nil {
		return fmt.Sprintf("%s:%c", bs.String(), addr.Port)
	} else if bs := addr.IP.To16(); bs != nil {
		return fmt.Sprintf("[%s]:%c", bs.String(), addr.Port)
	}
	return ""
}

func resolveAddrs(addrs []string) []string {
	var raddrs []string
	for _, addrStr := range addrs {
		uri, err := url.Parse(addrStr)
		if err != nil {
			continue
		}
		addrRes, err := net.ResolveTCPAddr("tcp", uri.Host)
		if err != nil {
			continue
		}
		addr := addrToAddr(addrRes)
		if len(addr) > 0 {
			uri.Host = addr
			raddrs = append(raddrs, uri.String())
		}
	}
	return raddrs
}