package dataset

import (
	"math"
	"slices"
	"sort"

	"github.com/sanjeev0303/object-storage-platform/internal/model"
)

// Drift: how a version's tables moved from a baseline's. Distributions are
// compared by the population stability index (PSI): numbers over the
// baseline's deciles, categories over the values either side lists.
// Rule of thumb: below 0.1 stable, 0.1 to 0.25 moving, above 0.25 shifted.

const minCompared = 20 // values each side needs before PSI means anything

func psi(before, after []float64) float64 {
	const eps = 1e-4
	total := 0.0
	for i := range before {
		p, q := math.Max(before[i], eps), math.Max(after[i], eps)
		total += (q - p) * math.Log(q/p)
	}
	return total
}

// numericPSI bins both samples by the baseline sample's deciles.
func numericPSI(base, cur []float64) float64 {
	sorted := slices.Clone(base)
	slices.Sort(sorted)
	var edges []float64
	for i := 1; i < 10; i++ {
		e := sorted[i*len(sorted)/10]
		if len(edges) == 0 || e > edges[len(edges)-1] {
			edges = append(edges, e)
		}
	}
	share := func(xs []float64) []float64 {
		out := make([]float64, len(edges)+1)
		for _, x := range xs {
			out[sort.Search(len(edges), func(k int) bool { return x < edges[k] })]++
		}
		for i := range out {
			out[i] /= float64(len(xs))
		}
		return out
	}
	return psi(share(base), share(cur))
}

// categoryPSI compares the listed values' shares, the rest as "other".
func categoryPSI(base, cur model.ColumnProfile) float64 {
	keys := map[string]bool{}
	for _, v := range base.Top {
		keys[v.Value] = true
	}
	for _, v := range cur.Top {
		keys[v.Value] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	share := func(c model.ColumnProfile) []float64 {
		out := make([]float64, len(names)+1)
		listed := int64(0)
		for i, name := range names {
			for _, v := range c.Top {
				if v.Value == name {
					out[i] = float64(v.Count)
					listed += v.Count
				}
			}
		}
		out[len(names)] = float64(c.Count - listed)
		for i := range out {
			out[i] /= float64(c.Count)
		}
		return out
	}
	return psi(share(base), share(cur))
}

func nullRate(c model.ColumnProfile) float64 {
	if c.Count+c.Nulls == 0 {
		return 0
	}
	return float64(c.Nulls) / float64(c.Count+c.Nulls)
}

func compareColumn(base, cur model.ColumnProfile, lim model.DriftLimits) model.ColumnDrift {
	d := model.ColumnDrift{Name: cur.Name, KindBefore: base.Kind, KindAfter: cur.Kind, NullRateBefore: nullRate(base),
		NullRateAfter: nullRate(cur), MeanBefore: base.Mean, MeanAfter: cur.Mean, Breached: []string{}}
	if base.Kind != cur.Kind && base.Kind != model.KindEmpty && cur.Kind != model.KindEmpty {
		d.Breached = append(d.Breached, "kind")
	}
	if math.Abs(d.NullRateAfter-d.NullRateBefore) > lim.MaxNullChange {
		d.Breached = append(d.Breached, "nulls")
	}
	numeric := func(k string) bool { return k == model.KindNumber || k == model.KindTime }
	switch {
	case base.Kind != cur.Kind:
		d.NotCompared = "the column's kind changed"
	case numeric(cur.Kind):
		if len(base.Sample) < minCompared || len(cur.Sample) < minCompared {
			d.NotCompared = "fewer than 20 values on one side"
			break
		}
		v := numericPSI(base.Sample, cur.Sample)
		d.PSI = &v
	case cur.Kind == model.KindString || cur.Kind == model.KindBool:
		if base.Count < minCompared || cur.Count < minCompared {
			d.NotCompared = "fewer than 20 values on one side"
			break
		}
		// Identifiers (nearly every value distinct) have no distribution
		// to compare.
		if base.DistinctCapped || cur.DistinctCapped || base.Distinct*2 > base.Count || cur.Distinct*2 > cur.Count {
			d.NotCompared = "values are nearly all distinct, like identifiers"
			break
		}
		v := categoryPSI(base, cur)
		d.PSI = &v
	default:
		d.NotCompared = "nested or empty values"
	}
	if d.PSI != nil && *d.PSI > lim.MaxPSI {
		d.Breached = append(d.Breached, "distribution")
	}
	return d
}

func compareProfiles(baseID, curID string, base, cur []model.TableProfile, lim model.DriftLimits) model.DriftReport {
	out := model.DriftReport{BaselineVersionID: baseID, VersionID: curID, Thresholds: lim, Tables: []model.TableDrift{}, NotCompared: []string{}}
	before := map[string]model.TableProfile{}
	for _, t := range base {
		before[t.Table] = t
	}
	seen := map[string]bool{}
	for _, t := range cur {
		seen[t.Table] = true
		b, ok := before[t.Table]
		if !ok {
			out.NotCompared = append(out.NotCompared, t.Table+" (new)")
			continue
		}
		td := model.TableDrift{Table: t.Table, RowsBefore: b.Rows, RowsAfter: t.Rows, ColumnsAdded: []string{}, ColumnsRemoved: []string{},
			Columns: []model.ColumnDrift{}, Breached: []string{}}
		td.RowChange = float64(t.Rows-b.Rows) / math.Max(float64(b.Rows), 1)
		if math.Abs(td.RowChange) > lim.MaxRowChange {
			td.Breached = append(td.Breached, "rows")
		}
		cols := map[string]model.ColumnProfile{}
		for _, c := range b.Columns {
			cols[c.Name] = c
		}
		present := map[string]bool{}
		for _, c := range t.Columns {
			present[c.Name] = true
			bc, ok := cols[c.Name]
			if !ok {
				td.ColumnsAdded = append(td.ColumnsAdded, c.Name)
				continue
			}
			cd := compareColumn(bc, c, lim)
			out.Breaches += len(cd.Breached)
			td.Columns = append(td.Columns, cd)
		}
		for _, c := range b.Columns {
			if !present[c.Name] {
				td.ColumnsRemoved = append(td.ColumnsRemoved, c.Name)
			}
		}
		if len(td.ColumnsRemoved) > 0 {
			td.Breached = append(td.Breached, "columns removed")
		}
		out.Breaches += len(td.Breached)
		out.Tables = append(out.Tables, td)
	}
	for _, t := range base {
		if !seen[t.Table] {
			// A table that disappeared is the plainest drift there is.
			out.NotCompared = append(out.NotCompared, t.Table+" (gone)")
			out.Breaches++
		}
	}
	return out
}
