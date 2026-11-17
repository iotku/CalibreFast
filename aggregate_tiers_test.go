package main

import (
	"slices"
	"testing"
)

func TestAggregateTiers(t *testing.T) {
	tierCounts := func(counts []int) [6]int {
		var n [6]int
		th := aggregateTiers(counts)
		for _, c := range counts {
			n[tierOf(th, c)]++
		}
		return n
	}

	// A long tail: one huge entry no longer shrinks everything else to the
	// smallest size, and each tier is a band of ranks.
	var long []int
	for i := 1; i <= 1000; i++ {
		long = append(long, 2+2000/i) // descending, as the cache sorts them
	}
	if !slices.IsSortedFunc(long, func(a, b int) int { return b - a }) {
		t.Fatal("fixture not descending")
	}
	n := tierCounts(long)
	for tier := 1; tier <= 5; tier++ {
		if n[tier] == 0 {
			t.Errorf("tier %d empty: %v", tier, n)
		}
	}
	if n[5] > 20 || n[1] < 500 {
		t.Errorf("tiers out of proportion: %v", n)
	}

	// All equal: all the same (smallest) size.
	if n := tierCounts([]int{2, 2, 2, 2}); n[1] != 4 {
		t.Errorf("equal counts: %v, want all tier 1", n)
	}

	// Few, distinct: more books is never a smaller tier, and the largest
	// stands out from the smallest.
	th := aggregateTiers([]int{9, 4, 3, 2})
	if !(tierOf(th, 9) >= tierOf(th, 4) && tierOf(th, 4) >= tierOf(th, 3) && tierOf(th, 3) >= tierOf(th, 2) && tierOf(th, 9) > tierOf(th, 2)) {
		t.Errorf("tiers not monotonic: %v", th)
	}

	// Thresholds strictly rise; single-book search results get tier 1.
	for i := 1; i < len(th); i++ {
		if th[i] <= th[i-1] {
			t.Errorf("thresholds not rising: %v", th)
		}
	}
	if tierOf(th, 1) != 1 {
		t.Errorf("tierOf(1) = %d, want 1", tierOf(th, 1))
	}
	_ = aggregateTiers(nil)
}
