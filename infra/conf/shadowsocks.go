package conf

import (
	"strings"

	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"google.golang.org/protobuf/proto"
)

// shadowsocks2022Methods mirrors shadowaead_2022.List from
// github.com/sagernet/sing-shadowsocks (GPL-3.0-or-later). This fork drops
// that dependency and the shadowsocks_2022 proxy along with it; the names are
// kept only so a config that asks for them gets a clear error instead of being
// silently parsed as a legacy Shadowsocks cipher.
var shadowsocks2022Methods = []string{
	"2022-blake3-aes-128-gcm",
	"2022-blake3-aes-256-gcm",
	"2022-blake3-chacha20-poly1305",
}

func isShadowsocks2022Method(c string) bool {
	for _, m := range shadowsocks2022Methods {
		if m == c {
			return true
		}
	}
	return false
}

func cipherFromString(c string) shadowsocks.CipherType {
	switch strings.ToLower(c) {
	case "aes-128-gcm", "aead_aes_128_gcm":
		return shadowsocks.CipherType_AES_128_GCM
	case "aes-256-gcm", "aead_aes_256_gcm":
		return shadowsocks.CipherType_AES_256_GCM
	case "chacha20-poly1305", "aead_chacha20_poly1305", "chacha20-ietf-poly1305":
		return shadowsocks.CipherType_CHACHA20_POLY1305
	case "xchacha20-poly1305", "aead_xchacha20_poly1305", "xchacha20-ietf-poly1305":
		return shadowsocks.CipherType_XCHACHA20_POLY1305
	case "none", "plain":
		return shadowsocks.CipherType_NONE
	default:
		return shadowsocks.CipherType_UNKNOWN
	}
}

type ShadowsocksUserConfig struct {
	Cipher   string   `json:"method"`
	Password string   `json:"password"`
	Level    byte     `json:"level"`
	Email    string   `json:"email"`
	Address  *Address `json:"address"`
	Port     uint16   `json:"port"`
}

type ShadowsocksServerConfig struct {
	Cipher      string                   `json:"method"`
	Password    string                   `json:"password"`
	Level       byte                     `json:"level"`
	Email       string                   `json:"email"`
	Users       []*ShadowsocksUserConfig `json:"clients"`
	NetworkList *NetworkList             `json:"network"`
}

func (v *ShadowsocksServerConfig) Build() (proto.Message, error) {
	errors.PrintNonRemovalDeprecatedFeatureWarning("Shadowsocks (with no Forward Secrecy, etc.)", "VLESS Encryption")

	if isShadowsocks2022Method(v.Cipher) {
		return nil, errors.New("Shadowsocks 2022 is not supported in this build")
	}

	config := new(shadowsocks.ServerConfig)
	config.Network = v.NetworkList.Build()

	if v.Users != nil {
		for _, user := range v.Users {
			account := &shadowsocks.Account{
				Password:   user.Password,
				CipherType: cipherFromString(user.Cipher),
			}
			if account.Password == "" {
				return nil, errors.New("Shadowsocks password is not specified.")
			}
			if account.CipherType < shadowsocks.CipherType_AES_128_GCM ||
				account.CipherType > shadowsocks.CipherType_XCHACHA20_POLY1305 {
				return nil, errors.New("unsupported cipher method: ", user.Cipher)
			}
			config.Users = append(config.Users, &protocol.User{
				Email:   user.Email,
				Level:   uint32(user.Level),
				Account: serial.ToTypedMessage(account),
			})
		}
	} else {
		account := &shadowsocks.Account{
			Password:   v.Password,
			CipherType: cipherFromString(v.Cipher),
		}
		if account.Password == "" {
			return nil, errors.New("Shadowsocks password is not specified.")
		}
		if account.CipherType == shadowsocks.CipherType_UNKNOWN {
			return nil, errors.New("unknown cipher method: ", v.Cipher)
		}
		config.Users = append(config.Users, &protocol.User{
			Email:   v.Email,
			Level:   uint32(v.Level),
			Account: serial.ToTypedMessage(account),
		})
	}

	return config, nil
}

type ShadowsocksServerTarget struct {
	Address    *Address `json:"address"`
	Port       uint16   `json:"port"`
	Level      byte     `json:"level"`
	Email      string   `json:"email"`
	Cipher     string   `json:"method"`
	Password   string   `json:"password"`
	UoT        bool     `json:"uot"`
	UoTVersion int      `json:"uotVersion"`
}

type ShadowsocksClientConfig struct {
	Address    *Address                   `json:"address"`
	Port       uint16                     `json:"port"`
	Level      byte                       `json:"level"`
	Email      string                     `json:"email"`
	Cipher     string                     `json:"method"`
	Password   string                     `json:"password"`
	UoT        bool                       `json:"uot"`
	UoTVersion int                        `json:"uotVersion"`
	Servers    []*ShadowsocksServerTarget `json:"servers"`
}

func (v *ShadowsocksClientConfig) Build() (proto.Message, error) {
	errors.PrintNonRemovalDeprecatedFeatureWarning("Shadowsocks (with no Forward Secrecy, etc.)", "VLESS Encryption")

	if v.Address != nil {
		v.Servers = []*ShadowsocksServerTarget{
			{
				Address:    v.Address,
				Port:       v.Port,
				Level:      v.Level,
				Email:      v.Email,
				Cipher:     v.Cipher,
				Password:   v.Password,
				UoT:        v.UoT,
				UoTVersion: v.UoTVersion,
			},
		}
	}
	if len(v.Servers) != 1 {
		return nil, errors.New(`Shadowsocks settings: "servers" should have one and only one member. Multiple endpoints in "servers" should use multiple Shadowsocks outbounds and routing balancer instead`)
	}

	if len(v.Servers) == 1 {
		server := v.Servers[0]
		if isShadowsocks2022Method(server.Cipher) {
			return nil, errors.New("Shadowsocks 2022 is not supported in this build")
		}
	}

	config := new(shadowsocks.ClientConfig)
	for _, server := range v.Servers {
		if isShadowsocks2022Method(server.Cipher) {
			return nil, errors.New("Shadowsocks 2022 is not supported in this build")
		}
		if server.Address == nil {
			return nil, errors.New("Shadowsocks server address is not set.")
		}
		if server.Port == 0 {
			return nil, errors.New("Invalid Shadowsocks port.")
		}
		if server.Password == "" {
			return nil, errors.New("Shadowsocks password is not specified.")
		}
		account := &shadowsocks.Account{
			Password: server.Password,
		}
		account.CipherType = cipherFromString(server.Cipher)
		if account.CipherType == shadowsocks.CipherType_UNKNOWN {
			return nil, errors.New("unknown cipher method: ", server.Cipher)
		}

		ss := &protocol.ServerEndpoint{
			Address: server.Address.Build(),
			Port:    uint32(server.Port),
			User: &protocol.User{
				Level:   uint32(server.Level),
				Email:   server.Email,
				Account: serial.ToTypedMessage(account),
			},
		}

		config.Server = ss
		break
	}

	return config, nil
}
