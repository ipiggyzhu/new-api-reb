package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestModelUpdateHandlerRunMarksScanFailureAsFailed pins that a model_update run
// whose channel scan query failed is recorded as failed, with the SQL error kept
// in the task row.
//
// This is a production regression test. A wrong column name in the scan's select
// list made every run fail at the query, but the handler still persisted
// "succeeded" with every counter at zero — indistinguishable in the task history
// from "there was nothing to check". Scheduled runs stayed broken for a day and
// the only trace was a backend log line nobody was watching.
func TestModelUpdateHandlerRunMarksScanFailureAsFailed(t *testing.T) {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	// system_tasks and its lock table exist so the run can be recorded; channels
	// is deliberately absent so the scan query fails the way a bad column did.
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))
	// A shared-cache in-memory database lives as long as one connection to it is
	// open, so leaving the pool open leaks the seeded rows into the next run of
	// this test in the same process and the seed below fails on the task_id
	// unique constraint under -count=2.
	t.Cleanup(func() {
		if sqlDB, sqlErr := db.DB(); sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	originalDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = originalDB })

	const runnerID = "test-runner"
	now := common.GetTimestamp()
	task := &model.SystemTask{
		TaskID:    "systask_scan_failure",
		Type:      model.SystemTaskTypeModelUpdate,
		Status:    model.SystemTaskStatusRunning,
		Payload:   `{"manual":true,"auto_apply":true}`,
		LockedBy:  runnerID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(task).Error)
	require.NoError(t, db.Create(&model.SystemTaskLock{
		Type:        model.SystemTaskTypeModelUpdate,
		TaskID:      task.TaskID,
		LockedBy:    runnerID,
		LockedUntil: now + 600,
		UpdatedAt:   now,
	}).Error)

	modelUpdateHandler{}.Run(context.Background(), task, runnerID)

	var persisted model.SystemTask
	require.NoError(t, db.Where("task_id = ?", task.TaskID).First(&persisted).Error)
	assert.Equal(t, model.SystemTaskStatusFailed, persisted.Status,
		"a run that could not read any channel must not be recorded as succeeded")
	assert.NotEmpty(t, persisted.Error, "the scan error must be kept on the task row")
}
