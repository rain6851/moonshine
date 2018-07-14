// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package freebsd

import (
	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/sys/freebsd/gen"
	"github.com/google/syzkaller/sys/targets"
)

func init() {
	prog.RegisterTarget(gen.Target_amd64, initTarget)
}

func initTarget(target *prog.Target) {
	arch := &arch{
		unix: targets.MakeUnixSanitizer(target),
	}

	target.MakeMmap = targets.MakePosixMmap(target)
	target.SanitizeCall = arch.unix.SanitizeCall
}

type arch struct {
	unix *targets.UnixSanitizer
}
