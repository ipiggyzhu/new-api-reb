package model

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// QuotaData 柱状图数据
type QuotaData struct {
	Id        int    `json:"id"`
	UserID    int    `json:"user_id" gorm:"index"`
	Username  string `json:"username" gorm:"index:idx_qdt_model_user_name,priority:2;size:64;default:''"`
	ModelName string `json:"model_name" gorm:"index:idx_qdt_model_user_name,priority:1;size:64;default:''"`
	CreatedAt int64  `json:"created_at" gorm:"bigint;index:idx_qdt_created_at,priority:2"`
	UseGroup  string `json:"use_group" gorm:"index;size:64;default:''"`
	TokenID   int    `json:"token_id" gorm:"index;default:0"`
	ChannelID int    `json:"channel_id" gorm:"index;default:0"`
	NodeName  string `json:"node_name" gorm:"index;size:64;default:''"`
	TokenUsed int    `json:"token_used" gorm:"default:0"`
	Count     int    `json:"count" gorm:"default:0"`
	Quota     int    `json:"quota" gorm:"default:0"`
}

type QuotaDataLogParams struct {
	UserID    int
	Username  string
	ModelName string
	Quota     int
	CreatedAt int64
	TokenUsed int
	UseGroup  string
	TokenID   int
	ChannelID int
	NodeName  string
}

// UpdateQuotaData flushes the dashboard quota aggregation on a fixed interval.
//
// The interval is floored at one minute: DataExportInterval reaches this loop
// from an admin option write, where a non-numeric value decodes to 0 and the
// backend applies no bound (only the settings form does). A zero would make
// time.Sleep return immediately and turn this into a tight loop over
// SaveQuotaDataCache, which writes to the same database the request path uses.
func UpdateQuotaData() {
	for {
		if common.DataExportEnabled {
			common.SysLog("正在更新数据看板数据...")
			SaveQuotaDataCache()
		}
		interval := common.DataExportInterval
		if interval < 1 {
			interval = 1
		}
		time.Sleep(time.Duration(interval) * time.Minute)
	}
}

var CacheQuotaData = make(map[string]*QuotaData)
var CacheQuotaDataLock = sync.Mutex{}

func logQuotaDataCache(quotaData *QuotaData) {
	key := fmt.Sprintf("%d\x00%s\x00%s\x00%d\x00%s\x00%d\x00%d\x00%s",
		quotaData.UserID,
		quotaData.Username,
		quotaData.ModelName,
		quotaData.CreatedAt,
		quotaData.UseGroup,
		quotaData.TokenID,
		quotaData.ChannelID,
		quotaData.NodeName,
	)
	count := quotaData.Count
	quota := quotaData.Quota
	tokenUsed := quotaData.TokenUsed
	cachedQuotaData, ok := CacheQuotaData[key]
	if ok {
		cachedQuotaData.Count += count
		cachedQuotaData.Quota += quota
		cachedQuotaData.TokenUsed += tokenUsed
		quotaData = cachedQuotaData
	}
	CacheQuotaData[key] = quotaData
}

func LogQuotaData(params QuotaDataLogParams) {
	// 只精确到小时
	createdAt := params.CreatedAt - (params.CreatedAt % 3600)
	quotaData := &QuotaData{
		UserID:    params.UserID,
		Username:  params.Username,
		ModelName: params.ModelName,
		CreatedAt: createdAt,
		UseGroup:  params.UseGroup,
		TokenID:   params.TokenID,
		ChannelID: params.ChannelID,
		NodeName:  params.NodeName,
		Count:     1,
		Quota:     params.Quota,
		TokenUsed: params.TokenUsed,
	}

	CacheQuotaDataLock.Lock()
	defer CacheQuotaDataLock.Unlock()
	logQuotaDataCache(quotaData)
}

// SaveQuotaDataCache flushes the dashboard aggregation to the database.
//
// The pending map is detached under the lock and the database work runs without
// it: LogQuotaData is on the request path and takes the same lock, so holding it
// across a round trip per cache key made every relay wait on the flush. Writers
// arriving during the flush accumulate into the fresh map and are picked up by
// the next one.
func SaveQuotaDataCache() {
	CacheQuotaDataLock.Lock()
	pending := CacheQuotaData
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()

	// increaseQuotaData reports whether the row existed, so the common case (the
	// hour bucket is already in the table) costs one UPDATE instead of a SELECT
	// followed by an UPDATE. Only a first-seen bucket pays a second round trip for
	// the INSERT.
	//
	// Each delta is applied exactly once whichever branch runs, and snapshots are
	// disjoint, so concurrent flushes cannot double-count. Two flushes racing on a
	// first-seen bucket can insert two rows for it; every reader aggregates
	// quota_data with SUM(...) GROUP BY, so the totals stay correct.
	for _, quotaData := range pending {
		updated, err := increaseQuotaData(quotaData)
		if err != nil {
			common.SysLog(fmt.Sprintf("increaseQuotaData error: %s", err))
			continue
		}
		if updated {
			continue
		}
		if err := DB.Table("quota_data").Create(quotaData).Error; err != nil {
			common.SysLog(fmt.Sprintf("createQuotaData error: %s", err))
		}
	}
	common.SysLog(fmt.Sprintf("保存数据看板数据成功，共保存%d条数据", len(pending)))
}

// increaseQuotaData adds the cached deltas onto the matching row and reports
// whether such a row existed.
//
// "Existed" is read off RowsAffected, which is only equivalent to "matched" here
// because every flushed bucket carries Count >= 1: LogQuotaData seeds Count at 1
// and logQuotaDataCache only ever adds to it, so the count + ? update always
// changes the row it matches. That distinction matters on MySQL, which reports
// changed rather than matched rows unless the connection sets CLIENT_FOUND_ROWS
// — a zero-delta bucket would be misread there as a missing row and inserted a
// second time. Any future path that can flush a bucket with Count == 0 has to
// switch this to an explicit existence check rather than rely on the delta.
func increaseQuotaData(quotaData *QuotaData) (bool, error) {
	result := DB.Table("quota_data").
		Where("user_id = ? and username = ? and model_name = ? and created_at = ? and use_group = ? and token_id = ? and channel_id = ? and node_name = ?",
			quotaData.UserID, quotaData.Username, quotaData.ModelName, quotaData.CreatedAt, quotaData.UseGroup, quotaData.TokenID, quotaData.ChannelID, quotaData.NodeName).
		Updates(map[string]interface{}{
			"count":      gorm.Expr("count + ?", quotaData.Count),
			"quota":      gorm.Expr("quota + ?", quotaData.Quota),
			"token_used": gorm.Expr("token_used + ?", quotaData.TokenUsed),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

func GetQuotaDataByUsername(username string, startTime int64, endTime int64) (quotaData []*QuotaData, err error) {
	var quotaDatas []*QuotaData
	// 从quota_data表中查询数据
	err = DB.Table("quota_data").
		Select("user_id, username, model_name, created_at, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used").
		Where("username = ? and created_at >= ? and created_at <= ?", username, startTime, endTime).
		Group("user_id, username, model_name, created_at").
		Find(&quotaDatas).Error
	return quotaDatas, err
}

func GetQuotaDataByUserId(userId int, startTime int64, endTime int64) (quotaData []*QuotaData, err error) {
	var quotaDatas []*QuotaData
	// 从quota_data表中查询数据
	err = DB.Table("quota_data").
		Select("user_id, username, model_name, created_at, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used").
		Where("user_id = ? and created_at >= ? and created_at <= ?", userId, startTime, endTime).
		Group("user_id, username, model_name, created_at").
		Find(&quotaDatas).Error
	return quotaDatas, err
}

func GetQuotaDataGroupByUser(startTime int64, endTime int64) (quotaData []*QuotaData, err error) {
	var quotaDatas []*QuotaData
	err = DB.Table("quota_data").
		Select("username, created_at, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used").
		Where("created_at >= ? and created_at <= ?", startTime, endTime).
		Group("username, created_at").
		Find(&quotaDatas).Error
	return quotaDatas, err
}

func GetAllQuotaDates(startTime int64, endTime int64, username string) (quotaData []*QuotaData, err error) {
	if username != "" {
		return GetQuotaDataByUsername(username, startTime, endTime)
	}
	var quotaDatas []*QuotaData
	// 从quota_data表中查询数据
	// only select model_name, sum(count) as count, sum(quota) as quota, model_name, created_at from quota_data group by model_name, created_at;
	//err = DB.Table("quota_data").Where("created_at >= ? and created_at <= ?", startTime, endTime).Find(&quotaDatas).Error
	err = DB.Table("quota_data").Select("model_name, sum(count) as count, sum(quota) as quota, sum(token_used) as token_used, created_at").Where("created_at >= ? and created_at <= ?", startTime, endTime).Group("model_name, created_at").Find(&quotaDatas).Error
	return quotaDatas, err
}
