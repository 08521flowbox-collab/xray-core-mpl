# What this fork is

A fork of [`XTLS/Xray-core`](https://github.com/XTLS/Xray-core) at tag
**`v1.260327.0`** with the **GPL-3.0-or-later dependencies removed**, so that the
result can be linked into an application without the GPL reaching that
application.

This repository exists to satisfy MPL-2.0 §3.2: it is the source of the Covered
Software embedded in the **ZapSplit** Android app, published so anyone with a
build can obtain and inspect it.

## Why

Upstream Xray-core is MPL-2.0, which is file-level copyleft and does not reach
code that merely calls it. But it depends unconditionally, in the **core
transport layer** (`transport/internet/system_dialer.go`,
`transport/internet/system_listener.go`), on `github.com/sagernet/sing` —
which is **GPL-3.0-or-later**. Anything that links Xray-core therefore links GPL
code, and the GPL's terms reach the whole combined work.

`github.com/sagernet/sing-shadowsocks`, needed by `proxy/shadowsocks_2022`, is
under the same licence.

Removing both is the entire purpose of this fork. There are no feature additions.

## What changed

See **[`MODIFICATIONS.md`](./MODIFICATIONS.md)** for the file-by-file list.

The history is deliberately split so the two kinds of change can be reviewed
separately:

| Commit | |
|---|---|
| `剥离 GPL 传递依赖：去掉 sagernet/sing 与 sing-shadowsocks` | the licence work — everything MPL-2.0 requires to be published |
| `为 APK 体积裁掉两块用不上的东西：DNS-over-QUIC 与 gRPC 传输凭据` | optional size trimming for the Android consumer |

To see the whole diff against the upstream release this is based on:

```sh
git diff v1.260327.0..mpl-only
```

## Verifying the claim

```sh
go list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./... | grep sagernet   # empty
grep sagernet go.mod go.sum                                                      # empty
```

Both are empty. The remaining dependency set is MIT / BSD-3 / Apache-2.0 /
MPL-2.0, with one exception worth naming: `github.com/juju/ratelimit`, reached
through `github.com/xtls/reality`, is LGPL-3.0 **with a static-linking
exception** that explicitly permits conveying a statically linked combined work
without providing minimal corresponding source.

## The module path is unchanged

`go.mod` still says `module github.com/xtls/xray-core`. That is deliberate:
renaming it would mean rewriting every internal import in the tree, and the
resulting diff would bury the four changes that actually matter. Consume this
fork with a directory `replace` (a git submodule, or a checkout next to your
own):

```
replace github.com/xtls/xray-core => ../path/to/xray-core-mpl
```

## Not affiliated with the Xray-core project

This is a third-party fork. The upstream project has not reviewed, endorsed or
approved it. Report bugs that reproduce on upstream to upstream.
