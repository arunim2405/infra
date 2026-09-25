package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/fc"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/uffd/userfaultfd"
	"github.com/e2b-dev/infra/packages/shared/pkg/sandboxtypes"
)

func TestBalloonModeOf(t *testing.T) {
	t.Parallel()
	assert.Equal(t, userfaultfd.BalloonModeReporting, balloonModeOf(fc.BalloonCaps{Reporting: true, Hinting: true}), "reporting wins: it is the mechanism that discards on its own")
	assert.Equal(t, userfaultfd.BalloonModeHinting, balloonModeOf(fc.BalloonCaps{Hinting: true}))
	assert.Equal(t, userfaultfd.BalloonModeNone, balloonModeOf(fc.BalloonCaps{}))
}

// A sandbox without a process (a build, a test) keeps the unknown label and
// the read is a no-op rather than a nil dereference.
func TestLabelBalloonMode_NoProcess(t *testing.T) {
	t.Parallel()
	s := &Sandbox{
		Metadata:  &Metadata{Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "mode-test"}},
		Resources: &Resources{memory: uffd.NewNoopMemory(1<<30, 2<<20)},
	}
	s.labelBalloonMode(t.Context())
	assert.Equal(t, "unknown", s.BalloonMode())

	s.setBalloonMode(userfaultfd.BalloonModeHinting)
	assert.Equal(t, "hinting", s.BalloonMode())
}

// A mode stamped from the template or the boot config is not re-derived: the
// device read only fills in unknown.
func TestLabelBalloonMode_KeepsKnownMode(t *testing.T) {
	t.Parallel()
	s := &Sandbox{
		Metadata:  &Metadata{Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "mode-known"}},
		Resources: &Resources{memory: uffd.NewNoopMemory(1<<30, 2<<20)},
	}
	s.setBalloonMode(userfaultfd.BalloonModeNone)
	s.labelBalloonMode(t.Context())
	assert.Equal(t, "none", s.BalloonMode())
}
