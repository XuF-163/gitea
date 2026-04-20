// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"code.gitea.io/gitea/modules/log"
)

// Backup represents the backup configuration
var Backup = struct {
	WebDAVStorage   *Storage
	Format          string
	SkipLFS         bool
	SkipAttachments bool
	SkipPackages    bool
	RetentionDays   int
}{}

func loadBackupFrom(rootCfg ConfigProvider) error {
	sec, err := rootCfg.GetSection("backup")
	if err != nil {
		// backup section is optional
		log.Info("Backup section not found in config, using defaults")
		return nil
	}

	Backup.Format = sec.Key("BACKUP_FORMAT").MustString("zip")
	Backup.SkipLFS = sec.Key("SKIP_LFS").MustBool(false)
	Backup.SkipAttachments = sec.Key("SKIP_ATTACHMENTS").MustBool(false)
	Backup.SkipPackages = sec.Key("SKIP_PACKAGES").MustBool(false)
	Backup.RetentionDays = sec.Key("RETENTION_DAYS").MustInt(7)

	if storageType := sec.Key("STORAGE_TYPE").String(); storageType == "" || storageType == "none" {
		Backup.WebDAVStorage = nil
		log.Info("Backup storage is disabled")
		return nil
	}

	// Load backup storage configuration
	Backup.WebDAVStorage, err = getStorage(rootCfg, "backup", "", sec)
	if err != nil {
		return err
	}

	log.Info("Backup storage type: %s", Backup.WebDAVStorage.Type)

	return nil
}
