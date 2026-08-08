# Modifications relative to upstream Xray-core

Base: [`XTLS/Xray-core`](https://github.com/XTLS/Xray-core) tag **`v1.260327.0`** (commit `d2758a0`).

This fork exists for exactly one reason: upstream Xray-core is MPL-2.0, but it depends on
`github.com/sagernet/sing` and `github.com/sagernet/sing-shadowsocks`, both of which are
**GPL-3.0-or-later**. Linking them into an application makes the whole binary subject to
the GPL. This fork removes those dependencies so the resulting library is MPL-2.0 only.

The fork is still MPL-2.0. Every file changed below is Covered Software and its source is
published here.

## Removing `github.com/sagernet/sing`

| File | Change |
|---|---|
| `common/control/interface.go` | **New.** Local definition of `control.Func`, previously taken from `sing/common/control`. It is a type alias of the standard library's `net.ListenConfig.Control` signature, so it is interchangeable with the original. |
| `transport/internet/system_dialer.go` | Import `sing/common/control` → `xray-core/common/control`. |
| `transport/internet/system_listener.go` | Same. |
| `transport/internet/system_listener_test.go` | Same. |
| `app/proxyman/outbound/uot.go` | **Deleted.** Implemented UDP-over-TCP on top of `sing/common/uot`, which is real GPL protocol code, not an alias. |
| `app/proxyman/outbound/handler.go` | Removed the `getUoTConnection` call and the now-unused `os` import. UoT only ever triggered for a destination domain equal to `uot.MagicAddress` / `uot.LegacyMagicAddress`, i.e. only for Shadowsocks-2022 clients, which this fork does not ship. |

## Removing `github.com/sagernet/sing-shadowsocks`

Shadowsocks-2022 cannot be implemented without it, so the protocol is dropped. Legacy
Shadowsocks (AEAD) is untouched and still works.

| File | Change |
|---|---|
| `proxy/shadowsocks_2022/` | **Deleted** (whole package). |
| `common/singbridge/` | **Deleted** (whole package). It existed only to adapt sing's interfaces for `proxy/shadowsocks_2022`; nothing else imported it. |
| `infra/conf/shadowsocks.go` | Dropped both sing imports and `buildShadowsocks2022`. `shadowaead_2022.List` is replaced by a local `shadowsocks2022Methods` list used solely to reject a 2022 method name with a clear error instead of silently mis-parsing it as a legacy cipher. |
| `main/commands/all/api/inbound_user_add.go` | Removed the `shadowsocks_2022.MultiUserServerConfig` case. |
| `testing/scenarios/shadowsocks_2022_test.go` | **Deleted.** |

## Removing `proxy/wireguard`

`proxy/wireguard/tun_linux.go` imports `sing/common/control`. WireGuard is not used by the
consumer of this fork, and keeping it would mean maintaining another sing-free port, so the
proxy is dropped rather than patched.

| File | Change |
|---|---|
| `proxy/wireguard/` | **Deleted** (whole package). |
| `infra/conf/wireguard.go`, `infra/conf/wireguard_test.go` | **Deleted.** |
| `infra/conf/xray.go` | Removed the two `"wireguard"` inbound/outbound config registrations. |
| `main/distro/all/all.go` | Removed the `proxy/wireguard` blank import. |
| `testing/scenarios/wireguard_test.go` | **Deleted.** |

Note that `transport/internet/finalmask/header/wireguard` is a different, unrelated package
(a packet header obfuscator) and is untouched.

## Verifying the result

```sh
go list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./... | grep sagernet   # must be empty
grep sagernet go.mod go.sum                                                      # must be empty
```

Both are empty as of this commit. The remaining dependency set is MIT / BSD / Apache-2.0 /
MPL-2.0, with one exception worth naming: `github.com/juju/ratelimit` (pulled in by
`github.com/xtls/reality`) is **LGPL-3.0 with a static-linking exception** that explicitly
permits conveying a statically linked Combined Work without providing minimal corresponding
source. Attribution is still required.
