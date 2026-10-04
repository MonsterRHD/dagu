// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !(darwin || linux)

package materialization

import "errors"

var errExchangeUnsupported = errors.New("atomic directory exchange is unsupported")

func exchangePaths(_, _ string) error {
	return errExchangeUnsupported
}
