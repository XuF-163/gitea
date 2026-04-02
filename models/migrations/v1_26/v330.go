// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"xorm.io/xorm"
)

type Repository struct { //revive:disable-line:exported
	ID         int64 `xorm:"pk autoincr"`
	IsInternal bool  `xorm:"INDEX NOT NULL DEFAULT false"`
}

// AddInternalVisibilityToRepository adds the is_internal column to the repository table.
func AddInternalVisibilityToRepository(x *xorm.Engine) error {
	if err := x.Sync(new(Repository)); err != nil {
		return err
	}

	// Ensure existing rows have deterministic values.
	_, err := x.Exec("UPDATE repository SET is_internal = ? WHERE is_internal IS NULL", false)
	return err
}

