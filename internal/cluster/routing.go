package cluster

import "strings"

// Route describes where an operation should execute.
type Route struct {
	Range  Range
	Addr   string
	Local  bool
	Leader string
}

// Router resolves table/key operations to ranges and nodes.
// It is intentionally decentralized: every node carries the same directory
// and routes independently, so there is no central query router.
type Router struct {
	ranges     *RangeStore
	membership *Membership
	self       string
}

// NewRouter creates a router over shared cluster state.
func NewRouter(ranges *RangeStore, membership *Membership, self string) *Router {
	return &Router{ranges: ranges, membership: membership, self: self}
}

// RouteKey returns the range owning key and the node that should serve it.
// Routing keys are table-qualified ("table/key") so future splits can place
// tables independently while today's single range owns everything.
func (r *Router) RouteKey(table, key string) (Route, error) {
	routingKey := table + "/" + key
	rng, err := r.ranges.Lookup(routingKey)
	if err != nil {
		return Route{}, err
	}
	return r.routeToRange(rng), nil
}

// RouteTable routes table-level operations (DDL, full scans) using the
// committed table assignment when present.
func (r *Router) RouteTable(table string) (Route, error) {
	lowered := strings.ToLower(table)
	assigned := r.ranges.TableRange(lowered)
	if assigned.ID != 0 {
		return r.routeToRange(assigned), nil
	}
	return r.RouteKey(lowered, "")
}

func (r *Router) routeToRange(rng Range) Route {
	target := rng.Leader
	if target == "" && len(rng.Replicas) > 0 {
		target = rng.Replicas[0]
	}
	addr, ok := r.membership.AddrOf(target)
	if !ok {
		addr = ""
	}
	return Route{Range: rng, Addr: addr, Local: target == r.self || (target == "" && rng.HasReplica(r.self)), Leader: target}
}

// indexFoldASCII is strings.Index(strings.ToUpper(s), sub) for ASCII
// needles without allocating; offsets stay valid on the original string
// even when it contains runes whose uppercase form differs in length.
func indexFoldASCII(s, sub string) int {
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			c := s[i+j]
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			if c != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// ExtractTableheuristically pulls a table name from SQL for routing.
// DDL and unknown shapes route by the default range.
func ExtractTable(sql string) string {
	for _, kw := range []string{"INTO ", "FROM ", "TABLE ", "UPDATE "} {
		if i := indexFoldASCII(sql, kw); i >= 0 {
			rest := sql[i+len(kw):]
			fields := strings.FieldsFunc(rest, func(c rune) bool {
				return c == ' ' || c == '\t' || c == '\n' || c == '(' || c == ';' || c == ','
			})
			if len(fields) > 0 {
				return strings.ToLower(fields[0])
			}
		}
	}
	return ""
}
