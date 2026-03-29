// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package storage

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_discardStorage(t *testing.T) {
	tests := []*discardStorage{
		uninitializedStorage.(*discardStorage),
		{reason: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			{
				got, err := tt.Open("path")
				assert.Nil(t, got)
				assert.Error(t, err)
			}
			{
				got, err := tt.Save("path", bytes.NewReader([]byte{0}), 1)
				assert.Equal(t, int64(0), got)
				assert.Error(t, err)
			}
			{
				got, err := tt.Stat("path")
				assert.Nil(t, got)
				assert.Error(t, err)
			}
			{
				err := tt.Delete("path")
				assert.Error(t, err)
			}
			{
				got, err := tt.ServeDirectURL("path", "name", "GET", nil)
				assert.Nil(t, got)
				assert.Error(t, err)
			}
			{
				err := tt.IterateObjects("", func(_ string, _ Object) error { return nil })
				assert.Error(t, err)
			}
		})
	}
}

func TestIsDiscardStorage(t *testing.T) {
	assert.True(t, IsDiscardStorage(uninitializedStorage))
	assert.True(t, IsDiscardStorage(&discardStorage{reason: "test"}))
	assert.False(t, IsDiscardStorage(nil))
}
