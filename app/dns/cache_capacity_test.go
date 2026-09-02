package dns

import (
	"fmt"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/net"
	"golang.org/x/net/dns/dnsmessage"
)

func TestCacheStaysAtCapacity(t *testing.T) {
	c := NewCacheController("test", false, false, 0)
	insert := func(domain string, ttl time.Duration) {
		c.updateRecord(&dnsRequest{reqType: dnsmessage.TypeA, domain: domain, start: time.Now()},
			&IPRecord{IP: []net.IP{net.ParseIP("10.0.0.1")}, Expire: time.Now().Add(ttl)})
	}
	for i := 0; i < cacheCapacity+51; i++ {
		insert(fmt.Sprintf("d%d.example", i), time.Hour)
	}
	c.RLock()
	size := len(c.ips)
	evictions := c.evictions
	c.RUnlock()
	if size != cacheCapacity {
		t.Fatalf("cache holds %d records, want %d", size, cacheCapacity)
	}
	if evictions != 51 {
		t.Fatalf("evictions = %d, want 51", evictions)
	}
	stale := &record{A: &IPRecord{Expire: time.Now().Add(time.Second)}, AAAA: &IPRecord{Expire: time.Now().Add(time.Hour)}}
	if !stale.expire().Before(time.Now().Add(time.Minute)) {
		t.Fatal("expire() should report the earlier of A and AAAA")
	}
}
