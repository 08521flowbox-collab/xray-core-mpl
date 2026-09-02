package strmatcher_test

import (
	"fmt"
	"testing"

	. "github.com/xtls/xray-core/common/strmatcher"
)

func TestDomainSetMatch(t *testing.T) {
	set := NewDomainSet([]string{"exact.example.com", "a.b"}, []string{"google.com", "cn", "sub.example.org"})
	cases := map[string]bool{
		"exact.example.com":     true,
		"www.exact.example.com": false,
		"a.b":                   true,
		"google.com":            true,
		"www.google.com":        true,
		"notgoogle.com":         false,
		"google.com.evil":       false,
		"baidu.cn":              true,
		"cn":                    true,
		"ncn":                   false,
		"sub.example.org":       true,
		"x.sub.example.org":     true,
		"example.org":           false,
		"":                      false,
	}
	for domain, want := range cases {
		if got := set.Match(domain); got != want {
			t.Errorf("Match(%q)=%v want %v", domain, got, want)
		}
	}
	if set.Size() != 5 {
		t.Errorf("Size=%d want 5", set.Size())
	}
}

func TestDomainSetEmpty(t *testing.T) {
	if NewDomainSet(nil, nil).Match("a.com") {
		t.Error("empty set matched")
	}
	var nilSet *DomainSet
	if nilSet.Match("a.com") {
		t.Error("nil set matched")
	}
}

func TestDomainSetLarge(t *testing.T) {
	var suffix, full []string
	for i := 0; i < 50000; i++ {
		suffix = append(suffix, fmt.Sprintf("s%d.example%d.com", i, i%97))
		full = append(full, fmt.Sprintf("f%d.example.net", i))
	}
	set := NewDomainSet(full, suffix)
	for i := 0; i < 50000; i += 997 {
		if !set.Match(fmt.Sprintf("deep.s%d.example%d.com", i, i%97)) {
			t.Errorf("suffix %d missing", i)
		}
		if !set.Match(fmt.Sprintf("f%d.example.net", i)) {
			t.Errorf("full %d missing", i)
		}
		if set.Match(fmt.Sprintf("x.f%d.example.net", i)) {
			t.Errorf("full %d matched a subdomain", i)
		}
	}
	if set.Match("s1.example1.org") || set.Match("example1.com") {
		t.Error("false positive")
	}
}
