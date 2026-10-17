package model

import (
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// repairChannelModelAvailability removes the retired model_availability map from
// channel settings. It hid models that were still listed on the channel from
// abilities, routing and the model plaza, and re-adding such a model did not
// clear it. A model is now unavailable only by being deleted from the channel's
// model list, so every affected channel gets its abilities rebuilt from that list.
func repairChannelModelAvailability(db *gorm.DB) error {
	var lastID, repaired int
	for {
		var ids []int
		if err := db.Model(&Channel{}).Where("id > ? AND (settings LIKE ? OR settings LIKE ?)", lastID, `%"model_availability"%`, `%"upstream_model_update_last_removed_models"%`).
			Order("id").Limit(200).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			lastID = id
			err := db.Transaction(func(tx *gorm.DB) error {
				var channel Channel
				if err := lockForUpdate(tx).First(&channel, id).Error; err != nil {
					return err
				}
				var fields map[string]json.RawMessage
				if err := common.UnmarshalJsonStr(channel.OtherSettings, &fields); err != nil {
					// Never log the settings themselves; they may contain credentials.
					common.SysLog(fmt.Sprintf("model availability repair skipped channel %d: invalid settings", id))
					return nil
				}
				_, quarantined := fields["model_availability"]
				_, pendingRemoval := fields["upstream_model_update_last_removed_models"]
				if !quarantined && !pendingRemoval {
					return nil
				}
				delete(fields, "model_availability")
				delete(fields, "upstream_model_update_last_removed_models")
				encoded, err := common.Marshal(fields)
				if err != nil {
					return err
				}
				channel.OtherSettings = string(encoded)
				if err := tx.Model(&Channel{}).Where("id = ?", id).UpdateColumn("settings", channel.OtherSettings).Error; err != nil {
					return err
				}
				if err := channel.UpdateAbilities(tx); err != nil {
					return err
				}
				repaired++
				return nil
			})
			if err != nil {
				return fmt.Errorf("repair model availability for channel %d: %w", id, err)
			}
		}
	}
	if repaired > 0 {
		common.SysLog(fmt.Sprintf("removed model availability quarantine and rebuilt abilities: %d channel(s)", repaired))
	}
	return nil
}
