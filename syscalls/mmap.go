package syscalls

import (
	"context"

	"github.com/evanphx/columbia/abi/linux"
	"github.com/evanphx/columbia/kernel"
	hclog "github.com/hashicorp/go-hclog"
)

func sysMmap(ctx context.Context, l hclog.Logger, p *kernel.Task, args SysArgs) int32 {
	var (
		ptr  = args.Args.R0
		size = args.Args.R1
		// prot   = args.Args.R2
		flags = args.Args.R3
		// fd     = args.Args.R4
		// offset = args.Args.R5

		// fixed    = flags&linux.MAP_FIXED != 0
		private = flags&linux.MAP_PRIVATE != 0
		shared  = flags&linux.MAP_SHARED != 0
		anon    = flags&linux.MAP_ANONYMOUS != 0
		// map32bit = flags&linux.MAP_32BIT != 0
	)

	// Require exactly one of MAP_PRIVATE and MAP_SHARED.
	if private == shared {
		return -kernel.EINVAL
	}

	if anon {
		ptr = -1
	}

	reg, err := p.Mem.NewRegion(ptr, size)
	if err != nil {
		return -kernel.EINVAL
	}

	return reg.Start
}

func init() {
	Syscalls[192] = sysMmap
}
