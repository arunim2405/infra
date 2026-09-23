package header

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestNewHeader_PageGranularMappingUnderHugepageBlockSize reproduces the
// production case: a memory file has BlockSize = 2 MiB (hugepage) but its diff
// mappings are page-granular (4 KiB), so the header carries 4 KiB-aligned
// offsets under a 2 MiB block size. The compact mapping must encode in PageSize
// units, not metadata.BlockSize, or header construction fails and bricks every
// sandbox create/resume.
func TestNewHeader_PageGranularMappingUnderHugepageBlockSize(t *testing.T) {
	t.Parallel()

	const hugepage = uint64(2 << 20) // 2 MiB memory block size
	a := uuid.New()
	b := uuid.New()

	// Page-granular (4 KiB) mappings, NOT aligned to the 2 MiB block size.
	mappings := []BuildMap{
		{Offset: 0, Length: PageSize, BuildId: a, BuildStorageOffset: 0},
		{Offset: PageSize, Length: PageSize, BuildId: b, BuildStorageOffset: 0},
		{Offset: 2 * PageSize, Length: 2 * PageSize, BuildId: a, BuildStorageOffset: PageSize},
	}
	meta := &Metadata{Version: MetadataVersionV4, BlockSize: hugepage, Size: 4 * PageSize, BuildId: a, BaseBuildId: b}

	h, err := NewHeader(meta, mappings)
	require.NoError(t, err, "page-granular mappings under a hugepage block size must be accepted")
	require.True(t, Equal(mappings, h.Mapping.Slice()))

	// And the offset lookup must resolve correctly at page granularity.
	m, err := h.GetShiftedMapping(t.Context(), PageSize)
	require.NoError(t, err)
	require.Equal(t, b, m.BuildId)
}

func TestNewMapping_RoundTrip(t *testing.T) {
	t.Parallel()

	bs := uint64(4096)
	a := uuid.New()
	b := uuid.New()
	src := []BuildMap{
		{Offset: 0, Length: 2 * bs, BuildId: a, BuildStorageOffset: 0},
		{Offset: 2 * bs, Length: bs, BuildId: b, BuildStorageOffset: 0},
		{Offset: 3 * bs, Length: bs, BuildId: a, BuildStorageOffset: 2 * bs},
	}

	m, err := NewMapping(bs, src)
	require.NoError(t, err)
	require.Equal(t, len(src), m.Len())

	require.True(t, Equal(src, m.Slice()), "Slice must round-trip the input")
	for i, want := range src {
		require.Equal(t, want, m.At(i), "At(%d)", i)
	}

	// Builds deduplicated to {a, b}.
	require.ElementsMatch(t, []uuid.UUID{a, b}, m.Builds())
}

func TestNewMapping_RejectsUnaligned(t *testing.T) {
	t.Parallel()

	bs := uint64(4096)
	id := uuid.New()

	_, err := NewMapping(bs, []BuildMap{{Offset: 123, Length: bs, BuildId: id}})
	require.ErrorContains(t, err, "offset")

	_, err = NewMapping(bs, []BuildMap{{Offset: 0, Length: 123, BuildId: id}})
	require.ErrorContains(t, err, "length")

	_, err = NewMapping(bs, []BuildMap{{Offset: 0, Length: bs, BuildId: id, BuildStorageOffset: 123}})
	require.ErrorContains(t, err, "build storage offset")

	_, err = NewMapping(0, []BuildMap{{Offset: 0, Length: bs, BuildId: id}})
	require.ErrorContains(t, err, "block size")
}

func TestMapping_SearchOffset(t *testing.T) {
	t.Parallel()

	bs := uint64(4096)
	id := uuid.New()
	src := []BuildMap{
		{Offset: 0, Length: 2 * bs, BuildId: id},
		{Offset: 2 * bs, Length: 2 * bs, BuildId: id, BuildStorageOffset: 2 * bs},
		{Offset: 4 * bs, Length: bs, BuildId: id, BuildStorageOffset: 4 * bs},
	}
	m, err := NewMapping(bs, src)
	require.NoError(t, err)

	// SearchOffset must match sort.Search over the materialized offsets for
	// every page within range, including non-block-aligned probes.
	for off := int64(0); off < int64(5*bs); off += int64(PageSize) {
		want := 0
		for _, bm := range src {
			if int64(bm.Offset) > off {
				break
			}
			want++
		}
		require.Equal(t, want, m.SearchOffset(off), "off=%d", off)
	}

	require.Equal(t, 0, m.SearchOffset(-1))
}

func TestMapping_Validate(t *testing.T) {
	t.Parallel()

	bs := uint64(4096)
	id := uuid.New()
	size := 4 * bs
	m, err := NewMapping(bs, []BuildMap{
		{Offset: 0, Length: 2 * bs, BuildId: id},
		{Offset: 2 * bs, Length: 2 * bs, BuildId: id, BuildStorageOffset: 2 * bs},
	})
	require.NoError(t, err)
	require.NoError(t, m.Validate(size, bs))

	// Wrong size is rejected.
	require.Error(t, m.Validate(size+bs, bs))

	// A gap is rejected.
	gap, err := NewMapping(bs, []BuildMap{
		{Offset: 0, Length: bs, BuildId: id},
		{Offset: 2 * bs, Length: bs, BuildId: id, BuildStorageOffset: 2 * bs},
	})
	require.NoError(t, err)
	require.Error(t, gap.Validate(3*bs, bs))
}

// ByteSize is what the orchestrator's residency gauges are built on, and the
// whole reason for the compact encoding is that these mappings dominate host
// RAM. A sign error or an off-by-one here would silently misreport the number
// the sizing decisions are made from, so pin the arithmetic against explicitly
// counted entries and builds.
func TestMapping_ByteSize(t *testing.T) {
	t.Parallel()

	const bytesPerEntry = 14 // offsets + lengths + storage (4 each) + buildIdx (2)
	const bytesPerBuild = 16 // uuid.UUID

	bs := uint64(4096)
	a := uuid.New()
	b := uuid.New()

	tests := []struct {
		name    string
		src     []BuildMap
		entries int
		builds  int
	}{
		{name: "empty"},
		{
			name:    "one entry one build",
			src:     []BuildMap{{Offset: 0, Length: bs, BuildId: a}},
			entries: 1,
			builds:  1,
		},
		{
			name: "builds are deduplicated, entries are not",
			src: []BuildMap{
				{Offset: 0, Length: bs, BuildId: a},
				{Offset: bs, Length: bs, BuildId: b},
				{Offset: 2 * bs, Length: bs, BuildId: a, BuildStorageOffset: bs},
			},
			entries: 3,
			builds:  2,
		},
		{
			// An empty region carries no build, so it costs an entry and nothing
			// in the build table.
			name: "nil build ids cost no build table slot",
			src: []BuildMap{
				{Offset: 0, Length: bs, BuildId: uuid.Nil},
				{Offset: bs, Length: bs, BuildId: uuid.Nil},
			},
			entries: 2,
			builds:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, err := NewMapping(bs, tt.src)
			require.NoError(t, err)

			require.Equal(t, tt.entries, m.Len())
			require.Len(t, m.Builds(), tt.builds)
			require.Equal(t, tt.entries*bytesPerEntry+tt.builds*bytesPerBuild, m.ByteSize())
		})
	}
}

// The encoding's claim is 14 bytes per entry against a BuildMap's 40, so the
// gauge must scale with entry count and stay far under the uncompacted size.
func TestMapping_ByteSizeScalesWithEntries(t *testing.T) {
	t.Parallel()

	bs := uint64(4096)
	id := uuid.New()

	src := make([]BuildMap, 1000)
	for i := range src {
		src[i] = BuildMap{Offset: uint64(i) * bs, Length: bs, BuildId: id}
	}

	m, err := NewMapping(bs, src)
	require.NoError(t, err)

	require.Equal(t, 1000*14+16, m.ByteSize())
	require.Less(t, m.ByteSize(), len(src)*40, "compact mapping must be smaller than the BuildMap slice it replaces")
}

// The footprint gauges rely on this to tell one allocation reached through two
// Headers from two allocations. CloneForUpload copies the Header struct, so the
// copy's Mapping shares the original's slices.
func TestMapping_SharesStorageWith(t *testing.T) {
	t.Parallel()

	bs := uint64(4096)
	id := uuid.New()
	src := []BuildMap{
		{Offset: 0, Length: bs, BuildId: id},
		{Offset: bs, Length: bs, BuildId: id, BuildStorageOffset: bs},
	}

	m, err := NewMapping(bs, src)
	require.NoError(t, err)

	shared := m
	require.True(t, m.SharesStorageWith(shared), "a copy shares the original's columns")
	require.True(t, shared.SharesStorageWith(m), "and the relation is symmetric")

	other, err := NewMapping(bs, src)
	require.NoError(t, err)
	require.False(t, m.SharesStorageWith(other), "separately built mappings are separate allocations")

	empty, err := NewMapping(bs, nil)
	require.NoError(t, err)
	require.False(t, empty.SharesStorageWith(empty), "an empty mapping holds no allocation to share")
	require.False(t, m.SharesStorageWith(empty))
}
