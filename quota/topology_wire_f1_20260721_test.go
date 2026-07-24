package quota

// T-G10 — F-1's Go twin, settled ON THE WIRE (pack 20260721; Python's Gate-D
// FAIL: redis-py 7.4.0 strips the CROSSSLOT prefix, so Python's substring
// probe was dead code and a REAL cluster was verdicted single-node under the
// operator assertion — under-refusal on the money path).
//
// The F-1 lesson is that a CLIENT LIBRARY rewrote what the server said, and
// the pinning test hand-raised the pre-parse string — green over an
// exception the real client never produces. My T-G3 tests in
// topology_probe_errors_20260721_test.go hand-construct errors the same way
// (fine as fast unit legs, but the SAME false-green shape). These tests
// close that: a minimal RESP server answers the REAL go-redis client with
// the REAL server bytes (`-CROSSSLOT …`, `-NOAUTH …`, `-NOPERM …`), and the
// verdict is asserted through CheckRedisClusterTopology end-to-end.
//
// Source facts (verified this session, recorded in the work log): the
// PINNED go-redis v9.6.1 surfaces error replies VERBATIM —
// proto.ParseErrorReply(line) → RedisError(line[1:]) — so the CROSSSLOT
// prefix survives; v9.21.0's typed ErrCrossSlot also embeds the prefix.
// The probe's substring match is therefore correct FOR THE WIRE FORMAT, and
// these tests pin exactly that: if a future go-redis rewraps server errors,
// they go RED where Python's stayed green.

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// fakeRespServer answers every parsed command via handler(cmd) with a raw
// RESP reply. Minimal RESP2 array parsing — enough for the client handshake
// (HELLO/CLIENT get error replies, which go-redis tolerates) and the probes.
func fakeRespServer(t *testing.T, handler func(cmd string) string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					if !strings.HasPrefix(line, "*") {
						continue
					}
					n, _ := strconv.Atoi(line[1:])
					var parts []string
					for i := 0; i < n; i++ {
						if _, err := r.ReadString('\n'); err != nil { // $<len>
							return
						}
						data, err := r.ReadString('\n')
						if err != nil {
							return
						}
						parts = append(parts, strings.TrimSpace(data))
					}
					if len(parts) == 0 {
						continue
					}
					if _, err := c.Write([]byte(handler(strings.ToUpper(parts[0])))); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return l.Addr().String()
}

func wireClient(t *testing.T, addr string) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The F-1 scenario itself, on the wire: admin probes ACL-denied, the data
// plane answers the REAL `-CROSSSLOT` bytes — the verdict must be CLUSTER,
// and the operator assertion must NOT mask it. (Python's equivalent
// verdicted single-node here. That is the money-path under-refusal.)
func TestWire_RealCrossslotReply_IsDefinitiveCluster(t *testing.T) {
	addr := fakeRespServer(t, func(cmd string) string {
		switch cmd {
		case "PING":
			return "+PONG\r\n"
		case "EXISTS":
			return "-CROSSSLOT Keys in request don't hash to the same slot\r\n"
		case "INFO":
			return "-NOPERM this user has no permissions to run the 'info' command\r\n"
		case "CLUSTER":
			return "-NOPERM this user has no permissions to run the 'cluster|info' command\r\n"
		default:
			return "-ERR unknown command\r\n"
		}
	})
	c := wireClient(t, addr)
	for _, confirmed := range []bool{false, true} {
		topo, detail := CheckRedisClusterTopology(context.Background(), c, confirmed)
		if topo != TopologyCluster {
			t.Errorf("F-1 Go twin (confirmed_disabled=%v): a REAL wire CROSSSLOT must verdict "+
				"CLUSTER, got %q (detail: %s) — under-refusing a real cluster certifies a store "+
				"whose every counter script will fail at the first acquire", confirmed, topo, detail)
		}
	}
}

// Auth on the wire: the real `-NOAUTH` bytes through the real client must be
// a probe FAILURE, never a topology verdict, and the assertion cannot mask it.
func TestWire_RealNoauthReply_IsProbeFailureNotVerdict(t *testing.T) {
	addr := fakeRespServer(t, func(cmd string) string {
		switch cmd {
		case "PING":
			return "+PONG\r\n" // reachable; auth denied at the probe stage
		default:
			return "-NOAUTH Authentication required.\r\n"
		}
	})
	c := wireClient(t, addr)
	topo, detail := CheckRedisClusterTopology(context.Background(), c, true)
	if topo != TopologyProbeFailed {
		t.Errorf("real wire NOAUTH must be PROBE FAILED (got %q, detail: %s)", topo, detail)
	}
}

// NOPERM everywhere with a WORKING data plane: the genuine absent-signal
// case, on the wire — unknown without the assertion, asserted single-node
// with it (the documented remedy, and the only state it is FOR).
func TestWire_RealNopermReply_IsAbsentSignal(t *testing.T) {
	addr := fakeRespServer(t, func(cmd string) string {
		switch cmd {
		case "PING":
			return "+PONG\r\n"
		case "EXISTS":
			return ":0\r\n"
		default:
			return "-NOPERM this user has no permissions\r\n"
		}
	})
	c := wireClient(t, addr)
	if topo, detail := CheckRedisClusterTopology(context.Background(), c, false); topo != TopologyUnknown {
		t.Errorf("wire NOPERM + working data plane must be UNKNOWN, got %q (%s)", topo, detail)
	}
	if topo, detail := CheckRedisClusterTopology(context.Background(), c, true); topo != TopologySingleNode {
		t.Errorf("the operator assertion is the documented remedy here, got %q (%s)", topo, detail)
	}
}
