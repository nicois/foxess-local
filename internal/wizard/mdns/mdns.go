// Package mdns answers "<host>.local" on the home network (design
// D-031, "Reaching the wizard"): A records only, for the stick's own
// IPv4 address. The OpenStick image runs no mDNS responder of its own.
package mdns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// DNS wire constants.
const (
	typeA      = 1
	typeAny    = 255
	classIN    = 1
	cacheFlush = 0x8000 // the class's top bit in a response
	unicastQU  = 0x8000 // the class's top bit in a question

	headerLen  = 12
	flagQR     = 0x8000 // a response
	flagAA     = 0x0400 // authoritative
	ttlSeconds = 120
	legacyTTL  = 10 // RFC 6762 §6.7: at most 10 s for one-shot queries

	maxLabel  = 63
	slack     = 32 // the record's fixed fields, rounded up
	ptrMask   = 0xc0
	ptrLen    = 2 // a compression pointer's size
	mdnsPort  = 5353
	maxPacket = 9000
	ipv4Len   = 4
)

var group = netip.MustParseAddr("224.0.0.251")

// localTLD is mDNS's domain.
const localTLD = "local"

// Answer builds the response to query when it asks for host.local
// (A or ANY). fromPort5353 is false for a legacy one-shot query, which
// gets a unicast reply carrying its id and question. unicast reports
// whether the reply goes back to the sender rather than the group.
func Answer(query []byte, host string, addr netip.Addr, fromPort5353 bool) (resp []byte, unicast, ok bool) {
	if !validHost(host) || len(query) < headerLen || binary.BigEndian.Uint16(query[2:])&flagQR != 0 || !addr.Is4() {
		return nil, false, false
	}
	qd := int(binary.BigEndian.Uint16(query[4:]))
	i := headerLen
	for range qd {
		name, next, err := readName(query, i)
		if err != nil || next+4 > len(query) {
			return nil, false, false
		}
		qtype := binary.BigEndian.Uint16(query[next:])
		qclass := binary.BigEndian.Uint16(query[next+2:])
		qEnd := next + 4
		if strings.EqualFold(name, host+"."+localTLD) && (qtype == typeA || qtype == typeAny) && qclass&^unicastQU == classIN {
			if !fromPort5353 {
				return legacyReply(query, i, qEnd, host, addr), true, true
			}
			return response(0, nil, host, addr, ttlSeconds, classIN|cacheFlush), qclass&unicastQU != 0, true
		}
		i = qEnd
	}
	return nil, false, false
}

// Announcement is the unsolicited response sent on joining a network;
// nil for an invalid host.
func Announcement(host string, addr netip.Addr) []byte {
	if !validHost(host) || !addr.Is4() {
		return nil
	}
	return response(0, nil, host, addr, ttlSeconds, classIN|cacheFlush)
}

// validHost: one DNS label.
func validHost(host string) bool {
	return host != "" && len(host) <= maxLabel && !strings.Contains(host, ".")
}

func legacyReply(query []byte, qStart, qEnd int, host string, addr netip.Addr) []byte {
	id := binary.BigEndian.Uint16(query[0:])
	q := append([]byte(nil), query[qStart:qEnd]...)
	// the question's QU bit is not echoed
	binary.BigEndian.PutUint16(q[len(q)-2:], classIN)
	return response(id, q, host, addr, legacyTTL, classIN)
}

func response(id uint16, question []byte, host string, addr netip.Addr, ttl uint32, class uint16) []byte {
	b := make([]byte, headerLen, headerLen+len(question)+len(host)+slack)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], flagQR|flagAA)
	if question != nil {
		binary.BigEndian.PutUint16(b[4:], 1)
	}
	binary.BigEndian.PutUint16(b[6:], 1)
	b = append(b, question...)
	for _, l := range []string{host, localTLD} {
		b = append(b, byte(len(l))) //nolint:gosec // G115: Answer and Announcement refuse a host label over 63 bytes
		b = append(b, l...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, typeA)
	b = binary.BigEndian.AppendUint16(b, class)
	b = binary.BigEndian.AppendUint32(b, ttl)
	b = binary.BigEndian.AppendUint16(b, ipv4Len)
	a := addr.As4()
	return append(b, a[:]...)
}

var errName = errors.New("mdns: malformed name")

// readName reads an uncompressed or compressed name at i; it returns
// the dotted name and the offset after it.
func readName(b []byte, i int) (string, int, error) {
	var labels []string
	end := -1
	for hops := 0; hops <= len(b); hops++ {
		if i >= len(b) {
			return "", 0, errName
		}
		l := int(b[i])
		if l&ptrMask == ptrMask {
			if i+1 >= len(b) {
				return "", 0, errName
			}
			end = firstEnd(end, i+ptrLen)
			i = int(binary.BigEndian.Uint16(b[i:]) &^ (ptrMask << 8))
			continue
		}
		if l == 0 {
			return strings.Join(labels, "."), firstEnd(end, i+1), nil
		}
		if l > maxLabel || i+1+l > len(b) {
			return "", 0, errName
		}
		labels = append(labels, string(b[i+1:i+1+l]))
		i += 1 + l
	}
	return "", 0, errName
}

// firstEnd keeps the offset after the name's first pointer, which is
// where the name ends in the message.
func firstEnd(end, at int) int {
	if end < 0 {
		return at
	}
	return end
}

// Responder answers for Host on one interface.
type Responder struct {
	Iface string               // wlan0
	Host  string               // foxess-1a2b
	Addr  func() netip.Addr    // the interface's current IPv4 address
	Log   func(string, ...any) // optional
}

// announceGap separates the two announcements on start (RFC 6762 §8.3).
const announceGap = time.Second

// Serve answers until ctx ends.
func (r *Responder) Serve(ctx context.Context) error {
	ifi, err := net.InterfaceByName(r.Iface)
	if err != nil {
		return fmt.Errorf("mdns: %w", err)
	}
	gaddr := &net.UDPAddr{IP: group.AsSlice(), Port: mdnsPort}
	conn, err := net.ListenMulticastUDP("udp4", ifi, gaddr)
	if err != nil {
		return fmt.Errorf("mdns: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	go r.announce(ctx, conn, gaddr)
	buf := make([]byte, maxPacket)
	for {
		n, src, rerr := conn.ReadFromUDP(buf)
		if rerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mdns: %w", rerr)
		}
		a := r.Addr()
		resp, unicast, ok := Answer(buf[:n], r.Host, a, src.Port == mdnsPort)
		if !ok {
			continue
		}
		dst := gaddr
		if unicast {
			dst = src
		}
		if _, werr := conn.WriteToUDP(resp, dst); werr != nil && r.Log != nil {
			r.Log("mdns: reply", "err", werr)
		}
	}
}

func (r *Responder) announce(ctx context.Context, conn *net.UDPConn, gaddr *net.UDPAddr) {
	for range 2 {
		if p := Announcement(r.Host, r.Addr()); p != nil {
			_, _ = conn.WriteToUDP(p, gaddr)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(announceGap):
		}
	}
}
