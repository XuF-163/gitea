// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package backup

import "sync"

// RestoreStatusType 表示恢复状态
type RestoreStatusType string

const (
	RestoreStatusIdle    RestoreStatusType = "idle"
	RestoreStatusRunning RestoreStatusType = "running"
	RestoreStatusDone    RestoreStatusType = "done"
	RestoreStatusFailed  RestoreStatusType = "failed"
)

// RestoreProgress 记录恢复进度
type RestoreProgress struct {
	Status  RestoreStatusType `json:"status"`
	Message string            `json:"message"`
	Percent int               `json:"percent"`
	Error   string            `json:"error,omitempty"`
}

var (
	restoreProgress     = &RestoreProgress{Status: RestoreStatusIdle}
	restoreProgressLock sync.RWMutex
)

// GetRestoreProgress 获取当前恢复进度
func GetRestoreProgress() *RestoreProgress {
	restoreProgressLock.RLock()
	defer restoreProgressLock.RUnlock()
	return &RestoreProgress{
		Status:  restoreProgress.Status,
		Message: restoreProgress.Message,
		Percent: restoreProgress.Percent,
		Error:   restoreProgress.Error,
	}
}

// UpdateRestoreProgress 更新恢复进度
func UpdateRestoreProgress(message string, percent int) {
	restoreProgressLock.Lock()
	defer restoreProgressLock.Unlock()
	restoreProgress.Status = RestoreStatusRunning
	restoreProgress.Message = message
	restoreProgress.Percent = percent
}

// SetRestoreDone 标记恢复完成
func SetRestoreDone() {
	restoreProgressLock.Lock()
	defer restoreProgressLock.Unlock()
	restoreProgress.Status = RestoreStatusDone
	restoreProgress.Message = "restore completed"
	restoreProgress.Percent = 100
}

// SetRestoreFailed 标记恢复失败
func SetRestoreFailed(errMsg string) {
	restoreProgressLock.Lock()
	defer restoreProgressLock.Unlock()
	restoreProgress.Status = RestoreStatusFailed
	restoreProgress.Error = errMsg
}

// ResetRestoreProgress 重置恢复进度
func ResetRestoreProgress() {
	restoreProgressLock.Lock()
	defer restoreProgressLock.Unlock()
	restoreProgress.Status = RestoreStatusIdle
	restoreProgress.Message = ""
	restoreProgress.Percent = 0
	restoreProgress.Error = ""
}
