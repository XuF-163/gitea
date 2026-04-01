// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"code.gitea.io/gitea/modules/setting/config"
)

// BackupConfigType 备份存储配置（在管理面板动态编辑）
// 字段名与模板中的表单 name 属性对应
type BackupConfigType struct {
	StorageType              string
	LocalPath                string
	WebDAVURL                string
	WebDAVUsername           string
	WebDAVPassword           string
	WebDAVTimeout            int
	WebDAVInsecureSkipVerify bool
}

// BackupSettingsStruct 备份设置动态选项
type BackupSettingsStruct struct {
	StorageConfig *config.Option[BackupConfigType]
	Format        *config.Option[string]
	SkipLFS       *config.Option[bool]
	SkipAttach    *config.Option[bool]
	SkipPackages  *config.Option[bool]
}
