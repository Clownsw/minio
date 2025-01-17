//go:build !linux
// +build !linux

// Copyright (c) 2015-2023 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"os"
)

// Rename2 is not implemented in a non linux environment
func Rename2(src, dst string) (err error) {
	return errSkipFile
}

// RenameSys is low level call in case of non-Linux this just uses os.Rename()
func RenameSys(src, dst string) (err error) {
	return os.Rename(src, dst)
}
