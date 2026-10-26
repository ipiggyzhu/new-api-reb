package controller

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// validateKeyEnvironmentSettings checks the independent key environment
// switch against the channel as it will be saved: fields the request leaves
// out keep their stored value.
func validateKeyEnvironmentSettings(channel *model.Channel, isMultiKey bool, previous *model.Channel) error {
	effective := *channel
	if previous != nil {
		if effective.Setting == nil {
			effective.Setting = previous.Setting
		}
		if effective.HeaderOverride == nil {
			effective.HeaderOverride = previous.HeaderOverride
		}
		if effective.Type == 0 {
			effective.Type = previous.Type
		}
	}
	setting := effective.GetSetting()
	if !setting.KeyEnvironments {
		return nil
	}
	if !isMultiKey {
		return fmt.Errorf("independent key environments need a multi-key channel")
	}
	family := service.KeyEnvironmentFamily(setting.SyntheticClientHeadersProfile)
	switch {
	case family == "":
		return fmt.Errorf("independent key environments need the claude or codex client profile")
	case family == constant.ClientHeaderFamilyClaude && effective.Type != constant.ChannelTypeAnthropic:
		return fmt.Errorf("claude key environments need an Anthropic channel")
	case family == constant.ClientHeaderFamilyCodex && effective.Type != constant.ChannelTypeOpenAI && effective.Type != constant.ChannelTypeCodex:
		return fmt.Errorf("codex key environments need an OpenAI channel")
	}
	if setting.KeyEnvironmentCacheTTL != "" && (family != constant.ClientHeaderFamilyClaude || setting.KeyEnvironmentCacheTTL != "1h") {
		return fmt.Errorf("unsupported key environment cache mode: %s", setting.KeyEnvironmentCacheTTL)
	}
	for name := range effective.GetHeaderOverride() {
		if service.IsProtectedProfileHeader(family, name) {
			return fmt.Errorf("header override %q would replace a header each key takes from its own CLI version or identity", name)
		}
	}
	return nil
}

// ensureKeyEnvironmentSeeds gives an independent key environment channel saved
// identity seeds. Without them identities derive from CryptoSecret, which is
// random per start when unset, so every restart would move every key to a new
// device. A seed the form left empty keeps the stored one.
func ensureKeyEnvironmentSeeds(channel *model.Channel, previous *model.Channel) error {
	if channel.Setting == nil {
		return nil
	}
	setting := channel.GetSetting()
	if !setting.KeyEnvironments || (setting.ClientDeviceSeed != "" && setting.ClientSessionSeed != "") {
		return nil
	}
	var stored = setting
	if previous != nil {
		stored = previous.GetSetting()
	}
	for _, seed := range []struct {
		value  *string
		stored string
	}{{&setting.ClientDeviceSeed, stored.ClientDeviceSeed}, {&setting.ClientSessionSeed, stored.ClientSessionSeed}} {
		if *seed.value != "" {
			continue
		}
		if seed.stored != "" {
			*seed.value = seed.stored
			continue
		}
		generated, err := newKeyEnvironmentSeed()
		if err != nil {
			return err
		}
		*seed.value = generated
	}
	channel.SetSetting(setting)
	return nil
}

func newKeyEnvironmentSeed() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// keyEnvironmentSaveHook reconciles key environments inside the channel save
// transaction. It is skipped while the switch is off and the key list did not
// change, the only case where there is nothing to assign and nothing to prune,
// so channels that never use the feature save exactly as before.
func keyEnvironmentSaveHook(allowUnassigned bool, keysChanged bool) func(tx *gorm.DB, saved *model.Channel) error {
	return func(tx *gorm.DB, saved *model.Channel) error {
		if !saved.ChannelInfo.IsMultiKey || (!saved.GetSetting().KeyEnvironments && !keysChanged) {
			return nil
		}
		_, err := service.ReconcileKeyEnvironments(tx, saved, allowUnassigned)
		return err
	}
}

// respondKeyEnvironmentError answers a rejected key environment plan with its
// structured detail so the form can explain it; other errors are left to the
// caller.
func respondKeyEnvironmentError(c *gin.Context, err error) bool {
	var duplicate *service.KeyEnvironmentDuplicateError
	var conflict *service.KeyEnvironmentConflictError
	var shortage *service.KeyEnvironmentShortageError
	switch {
	case errors.As(err, &duplicate):
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error(), "code": "key_environment_duplicate", "data": duplicate})
	case errors.As(err, &conflict):
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": err.Error(), "code": "key_environment_conflict", "data": conflict})
	case errors.As(err, &shortage):
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": err.Error(), "code": "key_environment_shortage", "data": shortage})
	default:
		return false
	}
	return true
}

// keyEnvironmentStatus is the per-key environment shown in the multi-key
// dialog: the assigned profile id by key index, read from the table rather
// than the cache so a save is visible at once.
func keyEnvironmentStatus(channel *model.Channel, keys []string) (map[int]string, error) {
	rows, err := model.LoadChannelKeyEnvironmentRows(model.DB, channel.Id)
	if err != nil {
		return nil, err
	}
	byTag := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ProfileId != nil {
			byTag[row.KeyTag] = *row.ProfileId
		}
	}
	profiles := make(map[int]string, len(keys))
	for i, key := range keys {
		profiles[i] = byTag[model.ChannelKeyTag(key)]
	}
	return profiles, nil
}
