// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"xorm.io/xorm"
)

type UserV331 struct { //revive:disable-line:exported
	ID        int64 `xorm:"pk autoincr"`
	UserLevel int   `xorm:"NOT NULL DEFAULT 0"`
}

func (*UserV331) TableName() string {
	return "user"
}

type RepositoryV331 struct { //revive:disable-line:exported
	ID                   int64 `xorm:"pk autoincr"`
	InternalMinUserLevel int   `xorm:"INDEX NOT NULL DEFAULT 0"`
}

func (*RepositoryV331) TableName() string {
	return "repository"
}

// AddUserLevelAndRepoInternalMinUserLevel adds the user_level column to the user table and
// internal_min_user_level column to the repository table.
func AddUserLevelAndRepoInternalMinUserLevel(x *xorm.Engine) error {
	if err := x.Sync(new(UserV331)); err != nil {
		return err
	}
	if err := x.Sync(new(RepositoryV331)); err != nil {
		return err
	}

	// Ensure existing rows have deterministic values.
	if _, err := x.Table("user").Where("user_level IS NULL").Cols("user_level").Update(&UserV331{UserLevel: 0}); err != nil {
		return err
	}
	_, err := x.Table("repository").Where("internal_min_user_level IS NULL").Cols("internal_min_user_level").Update(&RepositoryV331{InternalMinUserLevel: 0})
	return err
}
