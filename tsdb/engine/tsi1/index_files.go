package tsi1

import (
	"io"
	"sort"
)

// IndexFiles represents a layered set of index files.
type IndexFiles []*IndexFile

// MeasurementNames returns a sorted list of all measurement names for all files.
func (p *IndexFiles) MeasurementNames() [][]byte {
	itr := p.MeasurementIterator()
	var names [][]byte
	for e := itr.Next(); e != nil; e = itr.Next() {
		names = append(names, copyBytes(e.Name()))
	}
	sort.Sort(byteSlices(names))
	return names
}

// MeasurementIterator returns an iterator that merges measurements across all files.
func (p IndexFiles) MeasurementIterator() MeasurementIterator {
	a := make([]MeasurementIterator, len(p))
	for i := range p {
		a[i] = p[i].MeasurementIterator()
	}
	return MergeMeasurementIterators(a...)
}

// TagKeyIterator returns an iterator that merges tag keys across all files.
func (p *IndexFiles) TagKeyIterator(name []byte) (TagKeyIterator, error) {
	a := make([]TagKeyIterator, 0, len(*p))
	for _, f := range *p {
		itr, err := f.TagKeyIterator(name)
		if err != nil {
			return nil, err
		} else if itr == nil {
			continue
		}
		a = append(a, itr)
	}
	return MergeTagKeyIterators(a...), nil
}

// SeriesIterator returns an iterator that merges series across all files.
func (p IndexFiles) SeriesIterator() SeriesIterator {
	a := make([]SeriesIterator, len(p))
	for i := range p {
		a[i] = p[i].SeriesIterator()
	}
	return MergeSeriesIterators(a...)
}

// MeasurementSeriesIterator returns an iterator that merges series across all files.
func (p *IndexFiles) MeasurementSeriesIterator(name []byte) SeriesIterator {
	panic("TODO")
}

// TagValueSeriesIterator returns an iterator that merges series across all files.
func (p *IndexFiles) TagValueSeriesIterator(name, key, value []byte) SeriesIterator {
	panic("TODO")
}

// WriteTo merges all index files and writes them to w.
func (p *IndexFiles) WriteTo(w io.Writer) (n int64, err error) {
	var t IndexFileTrailer

	// Setup context object to track shared data for this compaction.
	var info indexCompactInfo
	info.tagSets = make(map[string]indexTagSetPos)
	info.names = p.MeasurementNames()

	// Write magic number.
	if err := writeTo(w, []byte(FileSignature), &n); err != nil {
		return n, err
	}

	// Write combined series list.
	t.SeriesBlock.Offset = n
	if err := p.writeSeriesBlockTo(w, &info, &n); err != nil {
		return n, err
	}
	t.SeriesBlock.Size = n - t.SeriesBlock.Offset

	// Write tagset blocks in measurement order.
	if err := p.writeTagsetsTo(w, &info, &n); err != nil {
		return n, err
	}

	// Write measurement block.
	t.MeasurementBlock.Offset = n
	if err := p.writeMeasurementBlockTo(w, &info, &n); err != nil {
		return n, err
	}
	t.MeasurementBlock.Size = n - t.MeasurementBlock.Offset

	// Write trailer.
	nn, err := t.WriteTo(w)
	n += nn
	if err != nil {
		return n, err
	}

	return n, nil
}

func (p *IndexFiles) writeSeriesBlockTo(w io.Writer, info *indexCompactInfo, n *int64) error {
	itr := p.SeriesIterator()

	// Write all series.
	sw := NewSeriesBlockWriter()
	for e := itr.Next(); e != nil; e = itr.Next() {
		if err := sw.Add(e.Name(), e.Tags()); err != nil {
			return err
		}
	}

	// Flush series list.
	nn, err := sw.WriteTo(w)
	*n += nn
	if err != nil {
		return err
	}

	// Attach writer to info so we can obtain series offsets later.
	info.sw = sw

	return nil
}

func (p *IndexFiles) writeTagsetsTo(w io.Writer, info *indexCompactInfo, n *int64) error {
	for _, name := range info.names {
		if err := p.writeTagsetTo(w, name, info, n); err != nil {
			return err
		}
	}
	return nil
}

// writeTagsetTo writes a single tagset to w and saves the tagset offset.
func (p *IndexFiles) writeTagsetTo(w io.Writer, name []byte, info *indexCompactInfo, n *int64) error {
	kitr, err := p.TagKeyIterator(name)
	if err != nil {
		return err
	}

	tw := NewTagBlockWriter()
	for ke := kitr.Next(); ke != nil; ke = kitr.Next() {
		// Mark tag deleted.
		if ke.Deleted() {
			tw.DeleteTag(ke.Key())
		}

		// Iterate over tag values.
		vitr := ke.TagValueIterator()
		for ve := vitr.Next(); ve != nil; ve = vitr.Next() {
			// Look-up series ids.
			sitr := ve.SeriesIterator()
			var seriesIDs []uint32
			for se := sitr.Next(); se != nil; se = sitr.Next() {
				seriesID := info.sw.Offset(se.Name(), se.Tags())
				if seriesID == 0 {
					panic("expected series id")
				}
				seriesIDs = append(seriesIDs, seriesID)
			}
			sort.Sort(uint32Slice(seriesIDs))

			// Insert tag value into writer.
			tw.AddTagValue(name, ve.Value(), ve.Deleted(), seriesIDs)
		}
	}

	// Save tagset offset to measurement.
	pos := info.tagSets[string(name)]
	pos.offset = *n

	// Write tagset to writer.
	nn, err := tw.WriteTo(w)
	*n += nn
	if err != nil {
		return err
	}

	// Save tagset size to measurement.
	pos.size = *n - pos.offset

	info.tagSets[string(name)] = pos

	return nil
}

func (p *IndexFiles) writeMeasurementBlockTo(w io.Writer, info *indexCompactInfo, n *int64) error {
	mw := NewMeasurementBlockWriter()

	// Add measurement data.
	for _, name := range info.names {
		// Look-up series ids.
		itr := p.MeasurementSeriesIterator(name)
		var seriesIDs []uint32
		for e := itr.Next(); e != nil; e = itr.Next() {
			seriesID := info.sw.Offset(e.Name(), e.Tags())
			if seriesID == 0 {
				panic("expected series id")
			}
			seriesIDs = append(seriesIDs, seriesID)
		}
		sort.Sort(uint32Slice(seriesIDs))

		// Add measurement to writer.
		pos := info.tagSets[string(name)]
		mw.Add(name, pos.offset, pos.size, seriesIDs)
	}

	// Write data to writer.
	nn, err := mw.WriteTo(w)
	*n += nn
	if err != nil {
		return err
	}

	return nil
}

// indexCompactInfo is a context object used for tracking position information
// during the compaction of index files.
type indexCompactInfo struct {
	// Sorted list of all measurements.
	// This is stored so it doesn't have to be recomputed.
	names [][]byte

	// Saved to look up series offsets.
	sw *SeriesBlockWriter

	// Tracks offset/size for each measurement's tagset.
	tagSets map[string]indexTagSetPos
}

// indexTagSetPos stores the offset/size of tagsets.
type indexTagSetPos struct {
	offset int64
	size   int64
}