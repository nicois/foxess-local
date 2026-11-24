package mdns

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

var addr = netip.MustParseAddr("192.0.2.40")

const host = "foxess-1a2b"

// query builds an mDNS question for name (labels), type qtype; qu sets
// the unicast-response bit.
func query(id uint16, name []string, qtype uint16, qu bool) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[4:], 1)
	for _, l := range name {
		b = append(b, byte(len(l))) //nolint:gosec // G115: the tests' own short labels
		b = append(b, l...)
	}
	b = append(b, 0)
	class := uint16(classIN)
	if qu {
		class |= 0x8000
	}
	b = binary.BigEndian.AppendUint16(b, qtype)
	return binary.BigEndian.AppendUint16(b, class)
}

// answerA pulls the first A record's address and TTL out of a response.
func answerA(t *testing.T, resp []byte) (netip.Addr, uint32, uint16) {
	t.Helper()
	if len(resp) < 12 || resp[2]&0x80 == 0 {
		t.Fatalf("not a response: % x", resp)
	}
	qd := binary.BigEndian.Uint16(resp[4:])
	an := binary.BigEndian.Uint16(resp[6:])
	if an != 1 {
		t.Fatalf("%d answers, want 1", an)
	}
	i := 12
	for range qd {
		i = skipName(resp, i) + 4
	}
	i = skipName(resp, i)
	typ := binary.BigEndian.Uint16(resp[i:])
	class := binary.BigEndian.Uint16(resp[i+2:])
	ttl := binary.BigEndian.Uint32(resp[i+4:])
	if typ != typeA || binary.BigEndian.Uint16(resp[i+8:]) != 4 {
		t.Fatalf("answer type %d", typ)
	}
	a, _ := netip.AddrFromSlice(resp[i+10 : i+14])
	return a, ttl, class
}

func skipName(b []byte, i int) int {
	for b[i] != 0 {
		if b[i]&0xc0 == 0xc0 {
			return i + 2
		}
		i += int(b[i]) + 1
	}
	return i + 1
}

func TestAnswerMulticast(t *testing.T) {
	q := query(0, []string{host, localTLD}, typeA, false)
	resp, unicast, ok := Answer(q, host, addr, true)
	if !ok || unicast {
		t.Fatalf("ok %v unicast %v", ok, unicast)
	}
	a, ttl, class := answerA(t, resp)
	if a != addr || ttl != ttlSeconds || class != classIN|cacheFlush {
		t.Fatalf("answer %s ttl %d class %#x", a, ttl, class)
	}
	if binary.BigEndian.Uint16(resp[4:]) != 0 {
		t.Fatal("a multicast response carries the question")
	}
}

func TestAnswerCaseInsensitive(t *testing.T) {
	q := query(0, []string{"FOXESS-1A2B", "Local"}, typeAny, false)
	if _, _, ok := Answer(q, host, addr, true); !ok {
		t.Fatal("a differently-cased name went unanswered")
	}
}

// TestAnswerLegacyUnicast: a one-shot query from a port other than 5353
// gets a unicast reply with its id and question (RFC 6762 §6.7).
func TestAnswerLegacyUnicast(t *testing.T) {
	q := query(0x4242, []string{host, localTLD}, typeA, false)
	resp, unicast, ok := Answer(q, host, addr, false)
	if !ok || !unicast {
		t.Fatalf("ok %v unicast %v", ok, unicast)
	}
	if binary.BigEndian.Uint16(resp[0:]) != 0x4242 || binary.BigEndian.Uint16(resp[4:]) != 1 {
		t.Fatalf("legacy reply lacks the id or the question: % x", resp[:12])
	}
	if a, ttl, class := answerA(t, resp); a != addr || ttl > legacyTTL || class&cacheFlush != 0 {
		t.Fatalf("legacy answer %s ttl %d class %#x", a, ttl, class)
	}
}

func TestAnswerQUBit(t *testing.T) {
	q := query(0, []string{host, localTLD}, typeA, true)
	if _, unicast, ok := Answer(q, host, addr, true); !ok || !unicast {
		t.Fatalf("QU question: ok %v unicast %v", ok, unicast)
	}
}

func TestAnswerIgnores(t *testing.T) {
	for name, q := range map[string][]byte{
		"other name": query(0, []string{"printer", localTLD}, typeA, false),
		"AAAA":       query(0, []string{host, localTLD}, 28, false),
		"short":      {0, 0, 0},
		"response":   append([]byte{0, 0, 0x84, 0}, query(0, []string{host, localTLD}, typeA, false)[4:]...),
		"overrun":    append(query(0, nil, typeA, false)[:12], 63, 'x'),
	} {
		if _, _, ok := Answer(q, host, addr, true); ok {
			t.Errorf("%s: answered", name)
		}
	}
}

func TestAnnouncement(t *testing.T) {
	a, ttl, _ := answerA(t, Announcement(host, addr))
	if a != addr || ttl != ttlSeconds {
		t.Fatalf("announcement %s ttl %d", a, ttl)
	}
}

func TestInvalidHost(t *testing.T) {
	q := query(0, []string{host, localTLD}, typeA, false)
	for _, h := range []string{"", "a.b", string(make([]byte, 64))} {
		if _, _, ok := Answer(q, h, addr, true); ok {
			t.Errorf("host %q answered", h)
		}
		if Announcement(h, addr) != nil {
			t.Errorf("host %q announced", h)
		}
	}
}
