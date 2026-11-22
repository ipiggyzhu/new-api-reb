package system_setting

import "github.com/QuantumNous/new-api/setting/config"

// Privacy is opt-in until an operator enables it for all relay requests.
// No credential or encryption key belongs in these database-backed options.
type PrivacySetting struct {
	Enabled              bool `json:"enabled"`
	ProtectIPs           bool `json:"protect_ips"`
	RestoreToolArguments bool `json:"restore_tool_arguments"`
}

var privacySetting = PrivacySetting{ProtectIPs: true}

func init() { config.GlobalConfig.Register("privacy_setting", &privacySetting) }

func GetPrivacySetting() *PrivacySetting { return &privacySetting }
