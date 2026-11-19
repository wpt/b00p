package cmd

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
)

// The subscriptions endpoint pages by offset; a blog on page 2 must be found,
// an empty page must stop the walk, and isPause / isSuspended must decode.
func TestFindSubscription(t *testing.T) {
	page := func(offset int, total int, blogs ...string) []byte {
		var data []map[string]any
		for _, b := range blogs {
			data = append(data, map[string]any{"name": "Tier " + b, "price": 300, "isPause": b == "two", "isSuspended": false, "blog": map[string]any{"blogUrl": b}})
		}
		out, _ := json.Marshal(map[string]any{"data": data, "total": total, "offset": offset, "limit": 1})
		return out
	}
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch off, _ := strconv.Atoi(r.URL.Query().Get("offset")); off {
		case 0:
			w.Write(page(0, 3, "one"))
		case 1:
			w.Write(page(1, 3, "two"))
		default:
			w.Write(page(off, 3)) // empty: a server ignoring offset must not loop
		}
	})

	sub, found, err := findSubscription(c, "TWO")
	if err != nil || !found || sub.Name != "Tier two" || !sub.IsPaused {
		t.Errorf("findSubscription(two) = %+v, %v, %v; want the page-2 entry, paused", sub, found, err)
	}
	if _, found, err := findSubscription(c, "absent"); err != nil || found {
		t.Errorf("findSubscription(absent) = %v, %v; want not found", found, err)
	}
	if n := calls.Load(); n != 2+3 {
		t.Errorf("requests = %d, want 5 (2 for the hit, 3 for the miss ending on the empty page)", n)
	}
	var s boosty.Subscription
	if err := json.Unmarshal([]byte(`{"isPause":true,"isSuspended":true}`), &s); err != nil || !s.IsPaused || !s.IsSuspended {
		t.Errorf("decoded %+v (%v), want both flags", s, err)
	}
}
