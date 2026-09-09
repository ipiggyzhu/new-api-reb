package model

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"gorm.io/gorm"
)

var errInvalidChannelClientSettings = errors.New("invalid channel settings")

// normalizeClientHeaderProfile materializes legacy defaults without replacing
// unrelated settings. Call only on private channel values being saved or migrated.
// Writers may ignore errInvalidChannelClientSettings so malformed historical
// settings do not block unrelated updates such as automatic channel disabling.
func (channel *Channel) normalizeClientHeaderProfile(db *gorm.DB) (bool, error) {
	if channel.Setting == nil || *channel.Setting == "" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	var settings dto.ChannelSettings
	if err := common.UnmarshalJsonStr(*channel.Setting, &fields); err != nil {
		return false, fmt.Errorf("%w for channel %d", errInvalidChannelClientSettings, channel.Id)
	}
	if err := common.UnmarshalJsonStr(*channel.Setting, &settings); err != nil {
		return false, fmt.Errorf("%w for channel %d", errInvalidChannelClientSettings, channel.Id)
	}
	oldProfile, oldEnabled := settings.SyntheticClientHeadersProfile, settings.SyntheticClientHeaders
	channelType := channel.Type
	needsDefault := (oldProfile != "" || oldEnabled) && oldProfile != "off" && !constant.IsClientHeaderFamily(oldProfile)
	if needsDefault && channelType == 0 && channel.Id != 0 {
		// Partial updates need the stored type; zero is an omitted field here.
		var existing Channel
		if err := db.Select("type").First(&existing, "id = ?", channel.Id).Error; err != nil {
			return false, err
		}
		channelType = existing.Type
	}
	apiType, _ := common.ChannelType2APIType(channelType)
	settings.Normalize(apiType)
	if settings.SyntheticClientHeadersProfile == oldProfile && settings.SyntheticClientHeaders == oldEnabled {
		return false, nil
	}
	profile, err := common.Marshal(settings.SyntheticClientHeadersProfile)
	if err != nil {
		return false, err
	}
	enabled, err := common.Marshal(settings.SyntheticClientHeaders)
	if err != nil {
		return false, err
	}
	fields["synthetic_client_headers_profile"] = profile
	fields["synthetic_client_headers"] = enabled
	encoded, err := common.Marshal(fields)
	if err != nil {
		return false, err
	}
	channel.Setting = common.GetPointer(string(encoded))
	return true, nil
}

func migrateChannelClientHeaderProfiles(db *gorm.DB) error {
	var lastID, updated int
	for {
		var channels []Channel
		if err := db.Select("id", "type", "setting").Where("id > ?", lastID).
			Order("id").Limit(200).Find(&channels).Error; err != nil {
			return err
		}
		if len(channels) == 0 {
			break
		}
		for i := range channels {
			channel := &channels[i]
			lastID = channel.Id
			original := channel.Setting
			changed, err := channel.normalizeClientHeaderProfile(db)
			if err != nil {
				if !errors.Is(err, errInvalidChannelClientSettings) {
					return err
				}
				// Preserve corrupt JSON for the explicit repair path. Never log it.
				common.SysLog(fmt.Sprintf("client profile migration skipped channel %d", channel.Id))
				continue
			}
			if !changed {
				continue
			}
			query := db.Model(&Channel{}).Where("id = ? AND type = ?", channel.Id, channel.Type)
			if db.Dialector.Name() == "mysql" {
				// The default MySQL text collation can ignore case and spaces.
				query = query.Where("BINARY setting = ?", *original)
			} else {
				query = query.Where("setting = ?", *original)
			}
			result := query.UpdateColumn("setting", channel.Setting)
			if result.Error != nil {
				return fmt.Errorf("migrate client profile for channel %d: %w", channel.Id, result.Error)
			}
			updated += int(result.RowsAffected)
		}
	}
	if updated > 0 {
		common.SysLog(fmt.Sprintf("materialized legacy channel client profiles: %d", updated))
	}
	return nil
}
