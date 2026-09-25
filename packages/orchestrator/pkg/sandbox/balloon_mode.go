package sandbox

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/userfaultfd"
	sbxlogger "github.com/e2b-dev/infra/packages/shared/pkg/logger/sandbox"
)

const balloonModeReadTimeout = 5 * time.Second

// labelBalloonMode reads which free-page mechanism the balloon runs and puts
// it on the sandbox and its serve metrics, for a sandbox whose template
// predates the metadata field. It runs off the start path, so such a
// sandbox's first faults after a resume carry unknown.
func (s *Sandbox) labelBalloonMode(ctx context.Context) {
	if s.process == nil || s.balloonMode.Load() != uint32(userfaultfd.BalloonModeUnknown) {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, balloonModeReadTimeout)
	defer cancel()

	caps, err := s.process.BalloonCaps(readCtx)
	if err != nil {
		if ctx.Err() == nil {
			sbxlogger.I(s).Warn(ctx, "balloon mode unread; serve metrics stay unknown", zap.Error(err))
		}

		return
	}
	s.setBalloonMode(balloonModeOf(caps))
}

func balloonModeOf(caps fc.BalloonCaps) userfaultfd.BalloonMode {
	switch {
	case caps.Reporting:
		return userfaultfd.BalloonModeReporting
	case caps.Hinting:
		return userfaultfd.BalloonModeHinting
	default:
		return userfaultfd.BalloonModeNone
	}
}

func (s *Sandbox) setBalloonMode(mode userfaultfd.BalloonMode) {
	s.balloonMode.Store(uint32(mode))
	if s.Resources == nil {
		return
	}
	if labeler, ok := s.Resources.memory.(uffd.BalloonModeLabeler); ok {
		labeler.SetBalloonMode(mode)
	}
}

// BalloonMode is the cohort label for spans and counters: reporting, hinting,
// none, or unknown before the read landed.
func (s *Sandbox) BalloonMode() string {
	return userfaultfd.BalloonMode(s.balloonMode.Load()).String()
}
