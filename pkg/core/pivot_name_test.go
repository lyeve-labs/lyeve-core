package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two names whose pivot is 70 characters and shares its first 63 with the
// pivot of longA and longC.
var (
	longA = "campaign_audience_segment_groups"
	longB = "newsletter_subscriber_profiles"
	longC = "newsletter_subscriber_profiles_archive"
)

func TestBoundedPivotTableName_KeepsANameThatFits(t *testing.T) {
	assert.Equal(t, "_pivot_articles_labels", BoundedPivotTableName("labels", "articles"))
	fits := strings.Repeat("a", 27)
	natural := PivotTableName(fits, fits+"b")
	require.Len(t, natural, PivotNameLimit)
	assert.Equal(t, natural, BoundedPivotTableName(fits, fits+"b"))
}

func TestBoundedPivotTableName_BoundsALongName(t *testing.T) {
	natural := PivotTableName(longA, longB)
	require.Greater(t, len(natural), PivotNameLimit)

	got := BoundedPivotTableName(longA, longB)
	assert.Len(t, got, PivotNameLimit)
	assert.True(t, strings.HasPrefix(got, natural[:PivotNameLimit-9]+"_"), got)
	assert.Equal(t, got, BoundedPivotTableName(longB, longA), "the order of the pair must not matter")

	other := BoundedPivotTableName(longA, longC)
	require.Equal(t, natural[:PivotNameLimit], PivotTableName(longA, longC)[:PivotNameLimit])
	assert.NotEqual(t, got, other, "two pairs sharing 63 characters must name two tables")
}

func TestResolvePivotTable_AskOnlyAboutALongName(t *testing.T) {
	asked := 0
	present := func(string) (bool, error) { asked++; return false, nil }

	got, err := ResolvePivotTable("articles", "labels", present)
	require.NoError(t, err)
	assert.Equal(t, "_pivot_articles_labels", got)
	assert.Zero(t, asked)

	got, err = ResolvePivotTable(longA, longB, present)
	require.NoError(t, err)
	assert.Equal(t, BoundedPivotTableName(longA, longB), got)
	assert.Equal(t, 1, asked)
}

func TestResolvePivotTable_KeepsAnExistingLongTable(t *testing.T) {
	natural := PivotTableName(longA, longB)
	got, err := ResolvePivotTable(longB, longA, func(name string) (bool, error) {
		assert.Equal(t, natural, name)
		return true, nil
	})
	require.NoError(t, err)
	assert.Equal(t, natural, got)
}

func TestResolvePivotTable_ReturnsTheCatalogError(t *testing.T) {
	boom := errors.New("catalog unreachable")
	_, err := ResolvePivotTable(longA, longB, func(string) (bool, error) { return false, boom })
	assert.ErrorIs(t, err, boom)
}
