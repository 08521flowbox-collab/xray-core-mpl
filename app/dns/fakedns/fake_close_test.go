package fakedns

import (
	"testing"

	"github.com/xtls/xray-core/common"
)

// Close used to nil out ipRange, domainToIP and mu, while the sniffer and the
// DNS client keep calling the engine from connections still in flight during
// instance teardown. One such call was a nil-pointer panic on a goroutine no
// recover reaches, which ends the whole process. Cherry-picked fix from
// upstream 7ab0a3ccb7; this pins the property the fix exists for.
func TestCloseLeavesTheEngineCallable(t *testing.T) {
	fkdns, err := NewFakeDNSHolder()
	common.Must(err)

	fake := fkdns.GetFakeIPForDomain("fakednstest.example.com")[0]
	common.Must(fkdns.Close())

	if !fkdns.IsIPInIPPool(fake) {
		t.Error("a fake IP stopped counting as one after Close")
	}
	if got := fkdns.GetDomainFromFakeDNS(fake); got != "fakednstest.example.com" {
		t.Errorf("GetDomainFromFakeDNS after Close = %q", got)
	}
	if got := fkdns.GetFakeIPForDomain3("fakednstest.example.com", true, false); len(got) == 0 {
		t.Error("GetFakeIPForDomain3 returned nothing after Close")
	}
}
