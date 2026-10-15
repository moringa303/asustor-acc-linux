package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Packets captured with `snmpget -d` against net-snmp's snmpd, asking for
// hrSystemUptime.0 and sysUpTime.0 in one request.
const (
	netsnmpV2cRequest  = `30 38 02 01 01 04 06 70 75 62 6C 69 63 A0 2B 02 04 1C 71 92 33 02 01 00 02 01 00 30 1D 30 0D 06 09 2B 06 01 02 01 19 01 01 00 05 00 30 0C 06 08 2B 06 01 02 01 01 03 00 05 00`
	netsnmpV2cResponse = `30 3E 02 01 01 04 06 70 75 62 6C 69 63 A2 31 02 04 1C 71 92 33 02 01 00 02 01 00 30 23 30 10 06 09 2B 06 01 02 01 19 01 01 00 43 03 00 DF EE 30 0F 06 08 2B 06 01 02 01 01 03 00 43 03 00 CC 05`
	netsnmpV1Request   = `30 38 02 01 00 04 06 70 75 62 6C 69 63 A0 2B 02 04 3A 97 98 2D 02 01 00 02 01 00 30 1D 30 0D 06 09 2B 06 01 02 01 19 01 01 00 05 00 30 0C 06 08 2B 06 01 02 01 01 03 00 05 00`
	// Asking for the nonexistent hrSystemUptime.99: v2c flags that one
	// varbind as noSuchInstance and answers the other ...
	netsnmpNoSuchInstance = `30 3B 02 01 01 04 06 70 75 62 6C 69 63 A2 2E 02 04 0E 56 57 A3 02 01 00 02 01 00 30 20 30 0D 06 09 2B 06 01 02 01 19 01 01 63 81 00 30 0F 06 08 2B 06 01 02 01 01 03 00 43 03 00 CE 6B`
	// ... while v1 rejects the whole request with noSuchName at index 1.
	netsnmpV1NoSuchName = `30 38 02 01 00 04 06 70 75 62 6C 69 63 A2 2B 02 04 3B F2 80 BD 02 01 02 02 01 01 30 1D 30 0D 06 09 2B 06 01 02 01 19 01 01 63 05 00 30 0C 06 08 2B 06 01 02 01 01 03 00 05 00`
)

func unhex(tb testing.TB, s string) []byte {
	tb.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

var bothOIDs = [][]uint32{oidHrSystemUptime, oidSysUpTime}

func TestEncodeGetRequestMatchesNetSNMP(t *testing.T) {
	tests := []struct {
		name    string
		version int64
		id      int32
		want    string
	}{
		{"v2c", snmpV2c, 0x1C719233, netsnmpV2cRequest},
		{"v1", snmpV1, 0x3A97982D, netsnmpV1Request},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeGetRequest(tc.version, "public", tc.id, bothOIDs)
			if want := unhex(t, tc.want); !bytes.Equal(got, want) {
				t.Errorf("got  %X\nwant %X", got, want)
			}
		})
	}
}

func TestDecodeNetSNMPResponses(t *testing.T) {
	r, err := decodeResponse(unhex(t, netsnmpV2cResponse))
	if err != nil {
		t.Fatal(err)
	}
	if r.version != snmpV2c || r.community != "public" || r.requestID != 0x1C719233 ||
		r.errorStatus != 0 || r.errorIndex != 0 || len(r.varbinds) != 2 {
		t.Fatalf("unexpected header: %+v", r)
	}
	if v, ok := r.ticks(oidHrSystemUptime); !ok || v != 57326 {
		t.Errorf("hrSystemUptime = %d, %v; want 57326", v, ok)
	}
	if v, ok := r.ticks(oidSysUpTime); !ok || v != 52229 {
		t.Errorf("sysUpTime = %d, %v; want 52229", v, ok)
	}

	r, err = decodeResponse(unhex(t, netsnmpNoSuchInstance))
	if err != nil {
		t.Fatal(err)
	}
	if r.requestID != 0x0E5657A3 || len(r.varbinds) != 2 || r.varbinds[0].tag != tagNoSuchInstance {
		t.Fatalf("unexpected noSuchInstance response: %+v", r)
	}
	if _, ok := r.ticks(oidHrSystemUptime); ok {
		t.Error("ticks found for an OID the agent flagged noSuchInstance")
	}
	if v, ok := r.ticks(oidSysUpTime); !ok || v != 52843 {
		t.Errorf("sysUpTime = %d, %v; want 52843", v, ok)
	}

	r, err = decodeResponse(unhex(t, netsnmpV1NoSuchName))
	if err != nil {
		t.Fatal(err)
	}
	if r.version != snmpV1 || r.errorStatus != 2 || r.errorIndex != 1 ||
		len(r.varbinds) != 2 || r.varbinds[0].tag != tagNull || r.varbinds[1].tag != tagNull {
		t.Fatalf("unexpected v1 noSuchName response: %+v", r)
	}
	if _, ok := r.ticks(oidSysUpTime); ok {
		t.Error("ticks found in an error response")
	}

	// The request decodes with the same parser given the request tag.
	req, err := decodeMessage(unhex(t, netsnmpV2cRequest), tagGetRequest)
	if err != nil {
		t.Fatal(err)
	}
	if req.requestID != 0x1C719233 || len(req.varbinds) != 2 || !oidEqual(req.varbinds[0].oid, oidHrSystemUptime) ||
		!oidEqual(req.varbinds[1].oid, oidSysUpTime) || req.varbinds[1].tag != tagNull {
		t.Errorf("unexpected request: %+v", req)
	}
	if _, err := decodeResponse(unhex(t, netsnmpV2cRequest)); err == nil {
		t.Error("a request decoded as a response")
	}
}

func TestBERInt(t *testing.T) {
	tests := []struct {
		v    int64
		want string
	}{
		{0, "02 01 00"}, {1, "02 01 01"}, {127, "02 01 7F"}, {128, "02 02 00 80"},
		{255, "02 02 00 FF"}, {256, "02 02 01 00"}, {-1, "02 01 FF"}, {-128, "02 01 80"},
		{-129, "02 02 FF 7F"}, {0x1C719233, "02 04 1C 71 92 33"},
		{math.MaxInt32, "02 04 7F FF FF FF"}, {math.MaxUint32, "02 05 00 FF FF FF FF"},
		{math.MaxInt64, "02 08 7F FF FF FF FF FF FF FF"}, {math.MinInt64, "02 08 80 00 00 00 00 00 00 00"},
	}
	for _, tc := range tests {
		got := berInt(tagInteger, tc.v)
		if want := unhex(t, tc.want); !bytes.Equal(got, want) {
			t.Errorf("berInt(%d) = %X, want %X", tc.v, got, want)
		}
		back, rest, err := readInt(got)
		if err != nil || back != tc.v || len(rest) != 0 {
			t.Errorf("readInt(%X) = %d, %v, %v; want %d", got, back, rest, err, tc.v)
		}
	}
	r := rand.New(rand.NewPCG(7, 11))
	for i := 0; i < 10000; i++ {
		v := int64(r.Uint64()) >> uint(r.IntN(64))
		if r.IntN(2) == 0 {
			v = -v
		}
		back, _, err := readInt(berInt(tagInteger, v))
		if err != nil || back != v {
			t.Fatalf("round trip of %d gave %d, %v", v, back, err)
		}
	}
	for _, bad := range [][]byte{nil, {}, bytes.Repeat([]byte{1}, 9)} {
		if _, err := decodeInt(bad); err == nil {
			t.Errorf("decodeInt(%X) accepted", bad)
		}
	}
}

func TestDecodeTicks(t *testing.T) {
	tests := []struct {
		val  string
		want uint32
		ok   bool
	}{
		{"00", 0, true}, {"00 DF EE", 57326, true}, {"7F FF FF FF", math.MaxInt32, true},
		{"00 FF FF FF FF", math.MaxUint32, true},
		// Agents that leave out the leading zero of a large value.
		{"FF FF FF FF", math.MaxUint32, true}, {"80 00 00 00", 1 << 31, true},
		{"", 0, false}, {"01 00 00 00 00", 0, false}, {"00 00 00 00 00 00", 0, false},
	}
	for _, tc := range tests {
		got, err := decodeTicks(unhex(t, tc.val))
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("decodeTicks(%s) = %d, %v; want %d, ok=%v", tc.val, got, err, tc.want, tc.ok)
		}
	}
}

func TestBEROID(t *testing.T) {
	tests := []struct {
		oid  []uint32
		want string
	}{
		{oidSysUpTime, "06 08 2B 06 01 02 01 01 03 00"},
		{oidHrSystemUptime, "06 09 2B 06 01 02 01 19 01 01 00"},
		{[]uint32{0, 0}, "06 01 00"},
		{[]uint32{1, 3, 6, 1, 4, 1, 2021}, "06 07 2B 06 01 04 01 8F 65"},
		{[]uint32{2, 100, math.MaxUint32}, "06 07 81 34 8F FF FF FF 7F"},
	}
	for _, tc := range tests {
		got := berOID(tc.oid)
		if want := unhex(t, tc.want); !bytes.Equal(got, want) {
			t.Errorf("berOID(%v) = %X, want %X", tc.oid, got, want)
		}
		val, rest, err := expectTLV(got, tagOID)
		if err != nil || len(rest) != 0 {
			t.Fatalf("expectTLV(%X): %v", got, err)
		}
		back, err := decodeOID(val)
		if err != nil || !oidEqual(back, tc.oid) {
			t.Errorf("decodeOID(%X) = %v, %v; want %v", val, back, err, tc.oid)
		}
	}
	for _, bad := range []string{"", "80", "2B 80", "2B FF FF FF FF 7F", "2B 90 80 80 80 00"} {
		if got, err := decodeOID(unhex(t, bad)); err == nil {
			t.Errorf("decodeOID(%s) = %v, want an error", bad, got)
		}
	}
	long := bytes.Repeat([]byte{1}, 200)
	if _, err := decodeOID(append([]byte{0x2B}, long...)); err == nil {
		t.Error("an OID of 200 arcs was accepted")
	}
}

func TestReadTLVRejects(t *testing.T) {
	bad := []string{
		"", "30", "30 80 02 01 00", // empty, no length, indefinite length
		"30 02 00", "30 81", "30 82 00 10 00", // length past the end
		"30 84 00 00 00 01 00", "30 80", // too many length bytes, indefinite
		"1F 01 00", "30 83 FF FF FF", // multi-byte tag, length past the end
	}
	for _, s := range bad {
		if _, _, _, err := readTLV(unhex(t, s)); err == nil {
			t.Errorf("readTLV(%s) accepted", s)
		}
	}
	// Long forms that are valid, including one longer than a datagram
	// as long as the bytes are really there.
	for _, n := range []int{0, 1, 127, 128, 255, 256, 3000, 70000} {
		val := bytes.Repeat([]byte{0xAB}, n)
		tag, got, rest, err := readTLV(append(berTLV(tagOctetString, val), 0x42))
		if err != nil || tag != tagOctetString || !bytes.Equal(got, val) || !bytes.Equal(rest, []byte{0x42}) {
			t.Errorf("readTLV of a %d-byte string: tag %#x, %d bytes, rest %X, %v", n, tag, len(got), rest, err)
		}
	}
}

// testResponse builds a GetResponse the way an agent would.
func testResponse(version int64, id int32, status, index int64, varbinds ...[]byte) []byte {
	var pdu []byte
	pdu = append(pdu, berInt(tagInteger, int64(id))...)
	pdu = append(pdu, berInt(tagInteger, status)...)
	pdu = append(pdu, berInt(tagInteger, index)...)
	pdu = append(pdu, berTLV(tagSequence, bytes.Join(varbinds, nil))...)
	var msg []byte
	msg = append(msg, berInt(tagInteger, version)...)
	msg = append(msg, berTLV(tagOctetString, []byte("public"))...)
	msg = append(msg, berTLV(tagGetResponse, pdu)...)
	return berTLV(tagSequence, msg)
}

func vbTicks(oid []uint32, ticks int64) []byte {
	return berTLV(tagSequence, append(berOID(oid), berInt(tagTimeTicks, ticks)...))
}

func vbEmpty(oid []uint32, tag byte) []byte {
	return berTLV(tagSequence, append(berOID(oid), berTLV(tag, nil)...))
}

func TestDecodeResponseRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		pkt    []byte
		id     int32
		status int64
		hr     int64 // -1: absent
		sys    int64
	}{
		{"both", testResponse(snmpV2c, 42, 0, 0, vbTicks(oidHrSystemUptime, 360000), vbTicks(oidSysUpTime, 100)), 42, 0, 360000, 100},
		{"max ticks", testResponse(snmpV2c, math.MaxInt32, 0, 0, vbTicks(oidHrSystemUptime, math.MaxUint32)), math.MaxInt32, 0, math.MaxUint32, -1},
		{"noSuchObject", testResponse(snmpV2c, 7, 0, 0, vbEmpty(oidHrSystemUptime, tagNoSuchObject), vbTicks(oidSysUpTime, 5)), 7, 0, -1, 5},
		{"endOfMibView", testResponse(snmpV2c, 7, 0, 0, vbEmpty(oidHrSystemUptime, tagEndOfMibView), vbEmpty(oidSysUpTime, tagNoSuchInstance)), 7, 0, -1, -1},
		{"error status", testResponse(snmpV1, 9, 5, 0, vbEmpty(oidSysUpTime, tagNull)), 9, 5, -1, -1},
		{"wrong type", testResponse(snmpV2c, 1, 0, 0, berTLV(tagSequence, append(berOID(oidSysUpTime), berTLV(tagOctetString, []byte("1 day"))...))), 1, 0, -1, -1},
		{"no varbinds", testResponse(snmpV2c, 1, 0, 0), 1, 0, -1, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := decodeResponse(tc.pkt)
			if err != nil {
				t.Fatalf("%X: %v", tc.pkt, err)
			}
			if r.requestID != tc.id || r.errorStatus != tc.status {
				t.Errorf("id %d status %d, want %d %d", r.requestID, r.errorStatus, tc.id, tc.status)
			}
			for _, o := range []struct {
				oid  []uint32
				want int64
			}{{oidHrSystemUptime, tc.hr}, {oidSysUpTime, tc.sys}} {
				v, ok := r.ticks(o.oid)
				if ok != (o.want >= 0) || (ok && int64(v) != o.want) {
					t.Errorf("ticks(%v) = %d, %v; want %d", o.oid, v, ok, o.want)
				}
			}
		})
	}
	bad := map[string][]byte{
		"trailing byte":     append(testResponse(snmpV2c, 1, 0, 0), 0),
		"negative id":       testResponse(snmpV2c, -1, 0, 0),
		"id beyond int32":   berTLV(tagSequence, bytes.Join([][]byte{berInt(tagInteger, 1), berTLV(tagOctetString, nil), berTLV(tagGetResponse, bytes.Join([][]byte{berInt(tagInteger, math.MaxInt32+1), berInt(tagInteger, 0), berInt(tagInteger, 0), berTLV(tagSequence, nil)}, nil))}, nil)),
		"varbind not a seq": testResponse(snmpV2c, 1, 0, 0, berTLV(tagOctetString, []byte("x"))),
		"varbind no value":  testResponse(snmpV2c, 1, 0, 0, berTLV(tagSequence, berOID(oidSysUpTime))),
		"varbind extra":     testResponse(snmpV2c, 1, 0, 0, berTLV(tagSequence, bytes.Join([][]byte{berOID(oidSysUpTime), berTLV(tagNull, nil), berTLV(tagNull, nil)}, nil))),
		"empty oid":         testResponse(snmpV2c, 1, 0, 0, berTLV(tagSequence, append(berTLV(tagOID, nil), berTLV(tagNull, nil)...))),
		"pdu is a request":  unhex(t, netsnmpV2cRequest),
	}
	for name, pkt := range bad {
		if r, err := decodeResponse(pkt); err == nil {
			t.Errorf("%s: accepted as %+v", name, r)
		}
	}
}

// TestDecodeGarbageDoesNotPanic feeds the decoder every truncation and
// single-byte mutation of a real response plus random bytes, bare and
// inside a plausible message skeleton. The test fails by panicking.
func TestDecodeGarbageDoesNotPanic(t *testing.T) {
	valid := unhex(t, netsnmpV2cResponse)
	for i := range valid {
		decodeResponse(valid[:i])
		mut := append([]byte(nil), valid...)
		for v := 0; v < 256; v++ {
			mut[i] = byte(v)
			decodeResponse(mut)
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	buf := make([]byte, maxDatagram)
	for i := 0; i < 20000; i++ {
		b := buf[:r.IntN(len(buf))+1]
		for j := range b {
			b[j] = byte(r.IntN(256))
		}
		decodeResponse(b)
		decodeResponse(berTLV(tagSequence, b))
		decodeResponse(testResponse(snmpV2c, 1, 0, 0, b))
		decodeResponse(testResponse(snmpV2c, 1, 0, 0, berTLV(tagSequence, b)))
		decodeResponse(testResponse(snmpV2c, 1, 0, 0, berTLV(tagSequence, append(berTLV(tagOID, b[:min(len(b), 40)]), berTLV(tagTimeTicks, b)...))))
		decodeOID(b[:min(len(b), 300)])
		decodeInt(b[:min(len(b), 12)])
		decodeTicks(b[:min(len(b), 8)])
		readTLV(b)
	}
}

func FuzzDecodeResponse(f *testing.F) {
	for _, s := range []string{netsnmpV2cResponse, netsnmpNoSuchInstance, netsnmpV1NoSuchName, netsnmpV2cRequest} {
		f.Add(unhex(f, s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if r, err := decodeResponse(data); err == nil && r == nil {
			t.Fatal("nil response without an error")
		}
		decodeMessage(data, tagGetRequest)
	})
}

// fakeAgent answers SNMP requests on the loopback interface the way
// handler says (one or more datagrams per request; none to stay silent).
// It returns the agent's address and a counter of requests by version.
func fakeAgent(t *testing.T, handler func(req *snmpResponse) [][]byte) (addr string, versions func() []int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	var (
		mu   sync.Mutex
		seen []int64
	)
	go func() {
		buf := make([]byte, maxDatagram)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req, err := decodeMessage(buf[:n], 0)
			if err == nil && req.pduTag == tagGetResponse {
				err = errMalformed
			}
			if err != nil {
				t.Errorf("agent received a malformed request %X: %v", buf[:n], err)
				continue
			}
			mu.Lock()
			seen = append(seen, req.version)
			mu.Unlock()
			for _, resp := range handler(req) {
				pc.WriteTo(resp, from)
			}
		}
	}()
	return pc.LocalAddr().String(), func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int64(nil), seen...)
	}
}

func answer(req *snmpResponse, varbinds ...[]byte) [][]byte {
	return [][]byte{testResponse(req.version, req.requestID, 0, 0, varbinds...)}
}

func TestSNMPUptimeClient(t *testing.T) {
	const timeout = 150 * time.Millisecond
	t.Run("hrSystemUptime preferred", func(t *testing.T) {
		addr, versions := fakeAgent(t, func(req *snmpResponse) [][]byte {
			return answer(req, vbTicks(oidHrSystemUptime, 360050), vbTicks(oidSysUpTime, 100))
		})
		secs, err := snmpUptime(addr, "public", timeout, 1)
		if err != nil || secs != 3600 {
			t.Fatalf("got %d, %v; want 3600", secs, err)
		}
		if v := versions(); len(v) != 1 || v[0] != snmpV2c {
			t.Errorf("requests seen: %v, want one v2c", v)
		}
	})
	t.Run("sysUpTime when the host MIB is missing", func(t *testing.T) {
		addr, _ := fakeAgent(t, func(req *snmpResponse) [][]byte {
			return answer(req, vbEmpty(oidHrSystemUptime, tagNoSuchObject), vbTicks(oidSysUpTime, 4200))
		})
		if secs, err := snmpUptime(addr, "public", timeout, 0); err != nil || secs != 42 {
			t.Fatalf("got %d, %v; want 42", secs, err)
		}
	})
	t.Run("v1 fallback after v2c retries", func(t *testing.T) {
		addr, versions := fakeAgent(t, func(req *snmpResponse) [][]byte {
			if req.version != snmpV1 {
				return nil
			}
			return answer(req, vbTicks(oidHrSystemUptime, 500), vbTicks(oidSysUpTime, 100))
		})
		start := time.Now()
		secs, err := snmpUptime(addr, "public", timeout, 1)
		if err != nil || secs != 5 {
			t.Fatalf("got %d, %v; want 5", secs, err)
		}
		if took := time.Since(start); took < 2*timeout {
			t.Errorf("answered after %s, before two v2c timeouts had passed", took)
		}
		if v := versions(); len(v) != 3 || v[0] != snmpV2c || v[1] != snmpV2c || v[2] != snmpV1 {
			t.Errorf("requests seen: %v, want v2c, v2c, v1", v)
		}
	})
	t.Run("v1 noSuchName retried with sysUpTime alone", func(t *testing.T) {
		addr, versions := fakeAgent(t, func(req *snmpResponse) [][]byte {
			if len(req.varbinds) != 1 {
				return [][]byte{testResponse(req.version, req.requestID, 2, 1,
					vbEmpty(oidHrSystemUptime, tagNull), vbEmpty(oidSysUpTime, tagNull))}
			}
			return answer(req, vbTicks(oidSysUpTime, 8640000))
		})
		if secs, err := snmpUptime(addr, "public", timeout, 0); err != nil || secs != 86400 {
			t.Fatalf("got %d, %v; want 86400", secs, err)
		}
		if v := versions(); len(v) != 2 {
			t.Errorf("requests seen: %v, want two", v)
		}
	})
	t.Run("stale and malformed replies are ignored", func(t *testing.T) {
		addr, _ := fakeAgent(t, func(req *snmpResponse) [][]byte {
			return [][]byte{
				[]byte("not snmp at all"),
				testResponse(req.version, req.requestID+1, 0, 0, vbTicks(oidHrSystemUptime, 999900)),
				testResponse(req.version, req.requestID, 0, 0, vbTicks(oidHrSystemUptime, 123400)),
			}
		})
		if secs, err := snmpUptime(addr, "public", timeout, 0); err != nil || secs != 1234 {
			t.Fatalf("got %d, %v; want 1234", secs, err)
		}
	})
	t.Run("error status without varbinds", func(t *testing.T) {
		addr, _ := fakeAgent(t, func(req *snmpResponse) [][]byte {
			return [][]byte{testResponse(req.version, req.requestID, 5, 0)}
		})
		if secs, err := snmpUptime(addr, "public", timeout, 0); err == nil {
			t.Fatalf("got %d, want an error", secs)
		}
	})
	t.Run("silence", func(t *testing.T) {
		addr, versions := fakeAgent(t, func(*snmpResponse) [][]byte { return nil })
		start := time.Now()
		_, err := snmpUptime(addr, "public", timeout, 1)
		took := time.Since(start)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
		// Two v2c attempts and one v1: three timeouts, no more.
		if took < 3*timeout || took > 3*timeout+time.Second {
			t.Errorf("gave up after %s, want about %s", took, 3*timeout)
		}
		if v := versions(); len(v) != 3 {
			t.Errorf("requests seen: %v, want three", v)
		}
	})
	t.Run("closed port fails at once", func(t *testing.T) {
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := pc.LocalAddr().String()
		pc.Close()
		start := time.Now()
		_, err = snmpUptime(addr, "public", 2*time.Second, 1)
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want a refused connection", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("took %s to notice the closed port", took)
		}
	})
}

func TestUptimeProbe(t *testing.T) {
	addr, versions := fakeAgent(t, func(req *snmpResponse) [][]byte {
		return answer(req, vbTicks(oidHrSystemUptime, 7200000))
	})
	_, port, _ := net.SplitHostPort(addr)
	defer func(p, c string, d bool) { snmpPort, snmpCommunity, snmpDisabled = p, c, d }(snmpPort, snmpCommunity, snmpDisabled)
	snmpPort, snmpCommunity, snmpDisabled = port, "public", false

	probe := newUptimeProbe()
	probe.ask("127.0.0.1", true)
	probe.ask("127.0.0.1", false) // already running: not asked twice
	probe.ask("", false)          // nothing to ask
	probe.ask("not an ip", false)
	list := []Nas{{Name: "A", IP: "127.0.0.1"}, {Name: "B"}, {Name: "C", IP: "127.0.0.2"}}
	probe.apply(list)
	if list[0].UptimeSeconds == nil || *list[0].UptimeSeconds != 72000 || list[0].UptimeSource != "snmp" || !list[0].SNMP {
		t.Errorf("A = %+v, want 72000 s from snmp", list[0])
	}
	for _, n := range list[1:] {
		if n.UptimeSeconds != nil || n.UptimeSource != "" || n.SNMP {
			t.Errorf("%s = %+v, want no uptime", n.Name, n)
		}
	}
	if v := versions(); len(v) != 1 {
		t.Errorf("requests seen: %v, want exactly one", v)
	}

	snmpDisabled = true
	probe = newUptimeProbe()
	probe.ask("127.0.0.1", true)
	list = []Nas{{Name: "A", IP: "127.0.0.1"}}
	probe.apply(list)
	if list[0].UptimeSeconds != nil || len(versions()) != 1 {
		t.Errorf("--no-snmp still asked: %+v, %v", list[0], versions())
	}
}

func TestDefaultCommunity(t *testing.T) {
	t.Setenv("ACC_SNMP_COMMUNITY", "")
	if got := defaultCommunity(); got != "public" {
		t.Errorf("got %q, want public", got)
	}
	t.Setenv("ACC_SNMP_COMMUNITY", "secret")
	if got := defaultCommunity(); got != "secret" {
		t.Errorf("got %q, want secret", got)
	}
}

// TestSNMPLive asks a real agent. Run it with
// ACC_SNMP_LIVE=127.0.0.1:161 (and ACC_SNMP_COMMUNITY when not public).
func TestSNMPLive(t *testing.T) {
	addr := os.Getenv("ACC_SNMP_LIVE")
	if addr == "" {
		t.Skip("set ACC_SNMP_LIVE=host:port to query a real SNMP agent")
	}
	secs, err := snmpUptime(addr, defaultCommunity(), snmpPatientTimeout, 1)
	if err != nil {
		t.Fatal(err)
	}
	if secs <= 0 {
		t.Fatalf("uptime %d s, want a positive one", secs)
	}
	t.Logf("%s reports an uptime of %s", addr, formatDuration(time.Duration(secs)*time.Second))
}
