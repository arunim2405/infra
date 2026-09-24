//go:build linux

package template

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block"
	blockmocks "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/block/mocks"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
	"github.com/e2b-dev/infra/packages/shared/pkg/utils"
)

// durableMemfileDevice is a ReadonlyDevice that also exposes DurableHeaderNow,
// like the real *Storage. Header() stands in for the live (provisional) header;
// DurableHeaderNow returns the deduped header scheduling metadata must use, and
// its ready flag models whether the deduped header has resolved yet.
type durableMemfileDevice struct {
	*blockmocks.MockReadonlyDevice

	durable *header.Header
	ready   bool
}

func (d durableMemfileDevice) DurableHeaderNow() (*header.Header, bool) {
	return d.durable, d.ready
}

func schedulingTemplate(t *testing.T, mem block.ReadonlyDevice, rootfsBase uuid.UUID) *storageTemplate {
	t.Helper()
	rootfsHdr, err := header.NewHeader(&header.Metadata{Version: 3, BlockSize: 4096, Size: 4096, BaseBuildId: rootfsBase}, nil)
	require.NoError(t, err)
	rootfsDev := blockmocks.NewMockReadonlyDevice(t)
	rootfsDev.EXPECT().Header().Return(rootfsHdr)

	tmpl := &storageTemplate{
		memfile: utils.NewSetOnce[block.ReadonlyDevice](),
		rootfs:  utils.NewSetOnce[block.ReadonlyDevice](),
	}
	require.NoError(t, tmpl.rootfs.SetValue(rootfsDev))
	require.NoError(t, tmpl.memfile.SetValue(mem))

	return tmpl
}

// When the deduped header has resolved, SchedulingMetadata reports it (never the
// live/provisional header) — so memfile build ids reflect the real build id.
func TestStorageTemplate_SchedulingMetadataUsesDurableHeader(t *testing.T) {
	t.Parallel()

	dedupedBase := uuid.New()
	dedupedHdr, err := header.NewHeader(&header.Metadata{Version: 3, BlockSize: 4096, Size: 4096, BaseBuildId: dedupedBase}, nil)
	require.NoError(t, err)
	memMock := blockmocks.NewMockReadonlyDevice(t)
	memMock.EXPECT().Header().Return(nil).Maybe()
	memDev := durableMemfileDevice{MockReadonlyDevice: memMock, durable: dedupedHdr, ready: true}

	md := schedulingTemplate(t, memDev, uuid.New()).SchedulingMetadata(t.Context())
	require.NotNil(t, md)
	assert.Equal(t, dedupedBase.String(), md.GetMemfileBaseBuildId())
}

// While the deduped header is still pending (provisional window), SchedulingMetadata
// must NOT block and must NOT emit the provisional build id: it reports rootfs-only
// metadata (empty memfile base build id).
func TestStorageTemplate_SchedulingMetadataSkipsPendingMemfile(t *testing.T) {
	t.Parallel()

	rootfsBase := uuid.New()
	memMock := blockmocks.NewMockReadonlyDevice(t)
	memMock.EXPECT().Header().Return(nil).Maybe()
	memDev := durableMemfileDevice{MockReadonlyDevice: memMock, durable: nil, ready: false}

	md := schedulingTemplate(t, memDev, rootfsBase).SchedulingMetadata(t.Context())
	require.NotNil(t, md)
	assert.Empty(t, md.GetMemfileBaseBuildId())
	assert.Equal(t, rootfsBase.String(), md.GetRootfsBaseBuildId())
}

// footprintHeader builds a header whose mapping has exactly n entries, using a
// distinct build id per entry so nothing merges.
func footprintHeader(t *testing.T, n int) *header.Header {
	t.Helper()

	const blockSize = uint64(4096)

	maps := make([]header.BuildMap, n)
	for i := range maps {
		maps[i] = header.BuildMap{
			Offset:  uint64(i) * blockSize,
			Length:  blockSize,
			BuildId: uuid.New(),
		}
	}

	h, err := header.NewHeader(&header.Metadata{
		Version:     3,
		BlockSize:   blockSize,
		Size:        uint64(n) * blockSize,
		BuildId:     uuid.New(),
		BaseBuildId: uuid.New(),
	}, maps)
	require.NoError(t, err)
	require.Equal(t, n, h.Mapping.Len())

	return h
}

// The gauge exists to size a bound, so it has to see every mapping the template
// keeps alive. After a pause the provisional header stays referenced by
// memfileHeader while the device has already moved on to the deduped one, so a
// gauge that reads only the devices reports one mapping where two are resident
// — understating exactly the footprint it is used to size.
func TestStorageTemplate_HeaderFootprintCountsRetainedHolders(t *testing.T) {
	t.Parallel()

	provisional := footprintHeader(t, 12)
	deduped := footprintHeader(t, 3)
	rootfsHdr := footprintHeader(t, 5)

	memDev := blockmocks.NewMockReadonlyDevice(t)
	memDev.EXPECT().Header().Return(deduped)
	rootfsDev := blockmocks.NewMockReadonlyDevice(t)
	rootfsDev.EXPECT().Header().Return(rootfsHdr)

	tmpl := &storageTemplate{
		memfile:              utils.NewSetOnce[block.ReadonlyDevice](),
		rootfs:               utils.NewSetOnce[block.ReadonlyDevice](),
		memfileHeader:        resolvedHeader(provisional),
		rootfsHeader:         resolvedHeader(rootfsHdr),
		durableMemfileHeader: resolvedHeader(deduped),
	}
	require.NoError(t, tmpl.memfile.SetValue(memDev))
	require.NoError(t, tmpl.rootfs.SetValue(rootfsDev))

	entries, bytes := tmpl.headerFootprint()

	wantEntries := provisional.Mapping.Len() + deduped.Mapping.Len() + rootfsHdr.Mapping.Len()
	wantBytes := provisional.Mapping.ByteSize() + deduped.Mapping.ByteSize() + rootfsHdr.Mapping.ByteSize()

	assert.Equal(t, wantEntries, entries, "the provisional header is still resident and must be counted")
	assert.Equal(t, wantBytes, bytes)
}

// The holders usually resolve to the same headers the devices carry. Counting
// those twice would overstate the number the sizing decisions are made from.
func TestStorageTemplate_HeaderFootprintCountsEachMappingOnce(t *testing.T) {
	t.Parallel()

	memHdr := footprintHeader(t, 7)
	rootfsHdr := footprintHeader(t, 4)

	memDev := blockmocks.NewMockReadonlyDevice(t)
	memDev.EXPECT().Header().Return(memHdr)
	rootfsDev := blockmocks.NewMockReadonlyDevice(t)
	rootfsDev.EXPECT().Header().Return(rootfsHdr)

	tmpl := &storageTemplate{
		memfile:       utils.NewSetOnce[block.ReadonlyDevice](),
		rootfs:        utils.NewSetOnce[block.ReadonlyDevice](),
		memfileHeader: resolvedHeader(memHdr),
		rootfsHeader:  resolvedHeader(rootfsHdr),
	}
	require.NoError(t, tmpl.memfile.SetValue(memDev))
	require.NoError(t, tmpl.rootfs.SetValue(rootfsDev))

	entries, bytes := tmpl.headerFootprint()

	assert.Equal(t, memHdr.Mapping.Len()+rootfsHdr.Mapping.Len(), entries)
	assert.Equal(t, memHdr.Mapping.ByteSize()+rootfsHdr.Mapping.ByteSize(), bytes)
}

// A template that is still fetching must contribute nothing rather than block
// the metrics collection goroutine on its unresolved futures.
func TestStorageTemplate_HeaderFootprintSkipsUnresolved(t *testing.T) {
	t.Parallel()

	tmpl := &storageTemplate{
		memfile:       utils.NewSetOnce[block.ReadonlyDevice](),
		rootfs:        utils.NewSetOnce[block.ReadonlyDevice](),
		memfileHeader: utils.NewSetOnce[*header.Header](),
		rootfsHeader:  utils.NewSetOnce[*header.Header](),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)

		entries, bytes := tmpl.headerFootprint()
		assert.Zero(t, entries)
		assert.Zero(t, bytes)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("headerFootprint blocked on an unresolved template")
	}
}

// One allocation reaches headerFootprint by two routes once a snapshot's upload
// publishes: CloneForUpload copies the Header struct, so the clone shares the
// source's Mapping slices, and publish installs the clone on the device while
// the holder keeps the source. Header identity does not see that, so the gauge
// would step up as uploads land — reading like retention growth rather than a
// counting artifact, on exactly the population a byte budget gets sized from.
func TestStorageTemplate_HeaderFootprintCountsSharedMappingOnce(t *testing.T) {
	t.Parallel()

	source := footprintHeader(t, 9)
	published := source.CloneForUpload(source.Metadata.Version + 1)
	require.NotSame(t, source, published, "the clone is a distinct header")
	require.True(t, source.Mapping.SharesStorageWith(published.Mapping),
		"the clone must share the source's mapping, or this test proves nothing")

	rootfsHdr := footprintHeader(t, 4)

	memDev := blockmocks.NewMockReadonlyDevice(t)
	memDev.EXPECT().Header().Return(published)
	rootfsDev := blockmocks.NewMockReadonlyDevice(t)
	rootfsDev.EXPECT().Header().Return(rootfsHdr)

	tmpl := &storageTemplate{
		memfile:       utils.NewSetOnce[block.ReadonlyDevice](),
		rootfs:        utils.NewSetOnce[block.ReadonlyDevice](),
		memfileHeader: resolvedHeader(source),
		rootfsHeader:  resolvedHeader(rootfsHdr),
	}
	require.NoError(t, tmpl.memfile.SetValue(memDev))
	require.NoError(t, tmpl.rootfs.SetValue(rootfsDev))

	entries, bytes := tmpl.headerFootprint()

	assert.Equal(t, source.Mapping.Len()+rootfsHdr.Mapping.Len(), entries,
		"the shared mapping must be counted once, not once per header")
	assert.Equal(t, source.Mapping.ByteSize()+rootfsHdr.Mapping.ByteSize(), bytes)
}

type countingFile struct {
	path   string
	closes atomic.Int32
}

func (f *countingFile) Path() string { return f.path }

func (f *countingFile) Close() error {
	f.closes.Add(1)

	return nil
}

// The cache can close one instance from two paths at once: a retired entry's
// last release and the eviction callback Invalidate queued. The teardown must
// run once however many callers race for it, and each caller must see it
// finished before Close returns.
func TestStorageTemplate_CloseRunsOnce(t *testing.T) {
	t.Parallel()

	paths, err := storage.Paths{BuildID: uuid.NewString()}.Cache(storage.Config{TemplateCacheDir: t.TempDir()})
	require.NoError(t, err)

	var memCloses, rootfsCloses atomic.Int32
	memDev := blockmocks.NewMockReadonlyDevice(t)
	memDev.EXPECT().Close().RunAndReturn(func() error {
		memCloses.Add(1)

		return nil
	}).Maybe()
	rootfsDev := blockmocks.NewMockReadonlyDevice(t)
	rootfsDev.EXPECT().Close().RunAndReturn(func() error {
		rootfsCloses.Add(1)

		return nil
	}).Maybe()
	snapfile := &countingFile{path: paths.CacheSnapfile()}

	tmpl := &storageTemplate{
		paths:    paths,
		memfile:  utils.NewSetOnce[block.ReadonlyDevice](),
		rootfs:   utils.NewSetOnce[block.ReadonlyDevice](),
		snapfile: utils.NewSetOnce[File](),
	}
	require.NoError(t, tmpl.memfile.SetValue(memDev))
	require.NoError(t, tmpl.rootfs.SetValue(rootfsDev))
	require.NoError(t, tmpl.snapfile.SetValue(snapfile))

	const callers = 8

	start := make(chan struct{})
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			<-start
			errs[i] = tmpl.Close(t.Context())
		})
	}

	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "caller %d", i)
	}

	assert.Equal(t, int32(1), memCloses.Load(), "memfile closed more than once")
	assert.Equal(t, int32(1), rootfsCloses.Load(), "rootfs closed more than once")
	assert.Equal(t, int32(1), snapfile.closes.Load(), "snapfile closed more than once")
	assert.NoDirExists(t, filepath.Dir(paths.CacheSnapfile()))
}
