// Copyright 2016 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package zoekt

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp/syntax"
	"sort"
	"strings"

	enry_data "github.com/go-enry/go-enry/v2/data"
	"github.com/grafana/regexp"

	"github.com/sourcegraph/zoekt/query"
)

const maxUInt16 = 0xffff

func (m *FileMatch) addScore(what string, s float64, debugScore bool) {
	if debugScore {
		m.Debug += fmt.Sprintf("%s:%.2f, ", what, s)
	}
	m.Score += s
}

func (m *FileMatch) addKeywordScore(score float64, sumTf float64, L float64, debugScore bool) {
	if debugScore {
		m.Debug += fmt.Sprintf("keyword-score:%.2f (sum-tf: %.2f, length-ratio: %.2f)", score, sumTf, L)
	}
	m.Score += score
}

// simplifyMultiRepo takes a query and a predicate. It returns Const(true) if all
// repository names fulfill the predicate, Const(false) if none of them do, and q
// otherwise.
func (d *indexData) simplifyMultiRepo(q query.Q, predicate func(*Repository) bool) query.Q {
	count := 0
	alive := len(d.repoMetaData)
	for i := range d.repoMetaData {
		if d.repoMetaData[i].Tombstone {
			alive--
		} else if predicate(&d.repoMetaData[i]) {
			count++
		}
	}
	if count == alive {
		return &query.Const{Value: true}
	}
	if count > 0 {
		return q
	}
	return &query.Const{Value: false}
}

func (d *indexData) simplify(in query.Q) query.Q {
	eval := query.Map(in, func(q query.Q) query.Q {
		switch r := q.(type) {
		case *query.Repo:
			return d.simplifyMultiRepo(q, func(repo *Repository) bool {
				return r.Regexp.MatchString(repo.Name)
			})
		case *query.RepoRegexp:
			return d.simplifyMultiRepo(q, func(repo *Repository) bool {
				return r.Regexp.MatchString(repo.Name)
			})
		case *query.BranchesRepos:
			for i := range d.repoMetaData {
				for _, br := range r.List {
					if br.Repos.Contains(d.repoMetaData[i].ID) {
						return q
					}
				}
			}
			return &query.Const{Value: false}
		case *query.RepoSet:
			return d.simplifyMultiRepo(q, func(repo *Repository) bool {
				return r.Set[repo.Name]
			})
		case *query.RepoIDs:
			return d.simplifyMultiRepo(q, func(repo *Repository) bool {
				return r.Repos.Contains(repo.ID)
			})
		case *query.Language:
			_, has := d.metaData.LanguageMap[r.Language]
			if !has && d.metaData.IndexFeatureVersion < 12 {
				// For index files that haven't been re-indexed by go-enry,
				// fall back to file-based matching and continue even if this
				// repo doesn't have the specific language present.
				extsForLang := enry_data.ExtensionsByLanguage[r.Language]
				if extsForLang != nil {
					extFrags := make([]string, 0, len(extsForLang))
					for _, ext := range extsForLang {
						extFrags = append(extFrags, regexp.QuoteMeta(ext))
					}
					if len(extFrags) > 0 {
						pattern := fmt.Sprintf("(?i)(%s)$", strings.Join(extFrags, "|"))
						// inlined copy of query.regexpQuery
						re, err := syntax.Parse(pattern, syntax.Perl)
						if err != nil {
							return &query.Const{Value: false}
						}
						if re.Op == syntax.OpLiteral {
							return &query.Substring{
								Pattern:  string(re.Rune),
								FileName: true,
							}
						}
						return &query.Regexp{
							Regexp:   re,
							FileName: true,
						}
					}
				}
			}
			if !has {
				return &query.Const{Value: false}
			}
		}
		return q
	})
	return query.Simplify(eval)
}

func (o *SearchOptions) SetDefaults() {
	if o.ShardMaxMatchCount == 0 {
		// We cap the total number of matches, so overly broad
		// searches don't crash the machine.
		o.ShardMaxMatchCount = 100000
	}
	if o.TotalMaxMatchCount == 0 {
		o.TotalMaxMatchCount = 10 * o.ShardMaxMatchCount
	}
}

func (d *indexData) Search(ctx context.Context, q query.Q, opts *SearchOptions) (sr *SearchResult, err error) {
	copyOpts := *opts
	opts = &copyOpts
	opts.SetDefaults()

	var res SearchResult
	if len(d.fileNameIndex) == 0 {
		return &res, nil
	}

	select {
	case <-ctx.Done():
		res.Stats.ShardsSkipped++
		return &res, nil
	default:
	}

	q = d.simplify(q)
	if c, ok := q.(*query.Const); ok && !c.Value {
		return &res, nil
	}

	if opts.EstimateDocCount {
		res.Stats.ShardFilesConsidered = len(d.fileBranchMasks)
		return &res, nil
	}

	q = query.Map(q, query.ExpandFileContent)

	mt, err := d.newMatchTree(q, matchTreeOpt{})
	if err != nil {
		return nil, err
	}

	// Capture the costs of construction before pruning
	updateMatchTreeStats(mt, &res.Stats)

	mt, err = pruneMatchTree(mt)
	if err != nil {
		return nil, err
	}
	if mt == nil {
		res.Stats.ShardsSkippedFilter++
		return &res, nil
	}

	res.Stats.ShardsScanned++

	cp := &contentProvider{
		id:    d,
		stats: &res.Stats,
	}

	// Track the number of documents found in a repository for
	// ShardRepoMaxMatchCount
	var (
		lastRepoID     uint16
		repoMatchCount int
	)

	docCount := uint32(len(d.fileBranchMasks))
	lastDoc := int(-1)

nextFileMatch:
	for {
		canceled := false
		select {
		case <-ctx.Done():
			canceled = true
		default:
		}

		nextDoc := mt.nextDoc()
		if int(nextDoc) <= lastDoc {
			nextDoc = uint32(lastDoc + 1)
		}

		for ; nextDoc < docCount; nextDoc++ {
			repoID := d.repos[nextDoc]
			repoMetadata := &d.repoMetaData[repoID]

			// Skip tombstoned repositories
			if repoMetadata.Tombstone {
				continue
			}

			// Skip documents that are tombstoned
			if len(repoMetadata.FileTombstones) > 0 {
				if _, tombstoned := repoMetadata.FileTombstones[string(d.fileName(nextDoc))]; tombstoned {
					continue
				}
			}

			// Skip documents over ShardRepoMaxMatchCount if specified.
			if opts.ShardRepoMaxMatchCount > 0 {
				if repoMatchCount >= opts.ShardRepoMaxMatchCount && repoID == lastRepoID {
					res.Stats.FilesSkipped++
					continue
				}
			}

			break
		}

		if nextDoc >= docCount {
			break
		}

		lastDoc = int(nextDoc)

		// We track lastRepoID for ShardRepoMaxMatchCount
		if lastRepoID != d.repos[nextDoc] {
			lastRepoID = d.repos[nextDoc]
			repoMatchCount = 0
		}

		if canceled || (res.Stats.MatchCount >= opts.ShardMaxMatchCount && opts.ShardMaxMatchCount > 0) {
			res.Stats.FilesSkipped += int(docCount - nextDoc)
			break
		}

		res.Stats.FilesConsidered++
		mt.prepare(nextDoc)

		cp.setDocument(nextDoc)

		known := make(map[matchTree]bool)
		md := d.repoMetaData[d.repos[nextDoc]]

		for cost := costMin; cost <= costMax; cost++ {
			v, ok := mt.matches(cp, cost, known)
			if ok && !v {
				continue nextFileMatch
			}

			if cost == costMax && !ok {
				log.Panicf("did not decide. Repo %s, doc %d, known %v",
					md.Name, nextDoc, known)
			}
		}

		fileMatch := FileMatch{
			Repository:         md.Name,
			RepositoryID:       md.ID,
			RepositoryPriority: md.priority,
			FileName:           string(d.fileName(nextDoc)),
			Checksum:           d.getChecksum(nextDoc),
			Language:           d.languageMap[d.getLanguage(nextDoc)],
		}

		if s := d.subRepos[nextDoc]; s > 0 {
			if s >= uint32(len(d.subRepoPaths[d.repos[nextDoc]])) {
				log.Panicf("corrupt index: subrepo %d beyond %v", s, d.subRepoPaths)
			}
			path := d.subRepoPaths[d.repos[nextDoc]][s]
			fileMatch.SubRepositoryPath = path
			sr := md.SubRepoMap[path]
			fileMatch.SubRepositoryName = sr.Name
			if idx := d.branchIndex(nextDoc); idx >= 0 {
				fileMatch.Version = sr.Branches[idx].Version
			}
		} else {
			idx := d.branchIndex(nextDoc)
			if idx >= 0 {
				fileMatch.Version = md.Branches[idx].Version
			}
		}

		shouldMergeMatches := !opts.ChunkMatches
		finalCands := gatherMatches(mt, known, shouldMergeMatches)

		if len(finalCands) == 0 {
			nm := d.fileName(nextDoc)
			finalCands = append(finalCands,
				&candidateMatch{
					caseSensitive: false,
					fileName:      true,
					substrBytes:   nm,
					substrLowered: nm,
					file:          nextDoc,
					runeOffset:    0,
					byteOffset:    0,
					byteMatchSz:   uint32(len(nm)),
				})
		}

		if opts.ChunkMatches {
			fileMatch.ChunkMatches = cp.fillChunkMatches(finalCands, opts.NumContextLines, fileMatch.Language, opts.DebugScore)
		} else {
			fileMatch.LineMatches = cp.fillMatches(finalCands, opts.NumContextLines, fileMatch.Language, opts.DebugScore)
		}

		if opts.UseKeywordScoring {
			d.scoreFileUsingBM25(&fileMatch, nextDoc, finalCands, opts)
		} else {
			// Use the standard, non-experimental scoring method by default
			d.scoreFile(&fileMatch, nextDoc, mt, known, opts)
		}

		fileMatch.Branches = d.gatherBranches(nextDoc, mt, known)
		sortMatchesByScore(fileMatch.LineMatches)
		sortChunkMatchesByScore(fileMatch.ChunkMatches)
		if opts.Whole {
			fileMatch.Content = cp.data(false)
		}

		matchedChunkRanges := 0
		for _, cm := range fileMatch.ChunkMatches {
			matchedChunkRanges += len(cm.Ranges)
		}

		repoMatchCount += len(fileMatch.LineMatches)
		repoMatchCount += matchedChunkRanges

		if opts.DebugScore {
			fileMatch.Debug = fmt.Sprintf("score:%.2f <- %s", fileMatch.Score, fileMatch.Debug)
		}

		res.Files = append(res.Files, fileMatch)
		res.Stats.MatchCount += len(fileMatch.LineMatches)
		res.Stats.MatchCount += matchedChunkRanges
		res.Stats.FileCount++
	}

	// We do not sort Files here, instead we rely on the shards pkg to do file
	// ranking. If we sorted now, we would break the assumption that results
	// from the same repo in a shard appear next to each other.

	for _, md := range d.repoMetaData {
		r := md
		addRepo(&res, &r)
		for _, v := range r.SubRepoMap {
			addRepo(&res, v)
		}
	}

	// Update stats based on work done during document search.
	updateMatchTreeStats(mt, &res.Stats)

	// If document ranking is enabled, then we can rank and truncate the files to save memory.
	if limit := opts.MaxDocDisplayCount; opts.UseDocumentRanks && limit > 0 && limit < len(res.Files) {
		SortFiles(res.Files)
		res.Files = res.Files[:limit]
	}

	return &res, nil
}

// scoreFile computes a score for the file match using various scoring signals, like
// whether there's an exact match on a symbol, the number of query clauses that matched, etc.
func (d *indexData) scoreFile(fileMatch *FileMatch, doc uint32, mt matchTree, known map[matchTree]bool, opts *SearchOptions) {
	atomMatchCount := 0
	visitMatches(mt, known, func(mt matchTree) {
		atomMatchCount++
	})

	// atom-count boosts files with matches from more than 1 atom. The
	// maximum boost is scoreFactorAtomMatch.
	if atomMatchCount > 0 {
		fileMatch.addScore("atom", (1.0-1.0/float64(atomMatchCount))*scoreFactorAtomMatch, opts.DebugScore)
	}

	maxFileScore := 0.0
	repetitions := 0
	for i := range fileMatch.LineMatches {
		if maxFileScore < fileMatch.LineMatches[i].Score {
			maxFileScore = fileMatch.LineMatches[i].Score
			repetitions = 0
		} else if maxFileScore == fileMatch.LineMatches[i].Score {
			repetitions += 1
		}

		// Order by ordering in file.
		fileMatch.LineMatches[i].Score += scoreLineOrderFactor * (1.0 - (float64(i) / float64(len(fileMatch.LineMatches))))
	}

	for i := range fileMatch.ChunkMatches {
		if maxFileScore < fileMatch.ChunkMatches[i].Score {
			maxFileScore = fileMatch.ChunkMatches[i].Score
		}

		// Order by ordering in file.
		fileMatch.ChunkMatches[i].Score += scoreLineOrderFactor * (1.0 - (float64(i) / float64(len(fileMatch.ChunkMatches))))
	}

	// Maintain ordering of input files. This
	// strictly dominates the in-file ordering of
	// the matches.
	fileMatch.addScore("fragment", maxFileScore, opts.DebugScore)

	// Prefer docs with several top-scored matches.
	fileMatch.addScore("repetition-boost", scoreRepetitionFactor*float64(repetitions), opts.DebugScore)

	if opts.UseDocumentRanks && len(d.ranks) > int(doc) {
		weight := scoreFileRankFactor
		if opts.DocumentRanksWeight > 0.0 {
			weight = opts.DocumentRanksWeight
		}

		ranks := d.ranks[doc]
		// The ranks slice always contains one entry representing the file rank (unless it's empty since the
		// file doesn't have a rank). This is left over from when documents could have multiple rank signals,
		// and we plan to clean this up.
		if len(ranks) > 0 {
			// The file rank represents a log (base 2) count. The log ranks should be bounded at 32, but we
			// cap it just in case to ensure it falls in the range [0, 1].
			normalized := math.Min(1.0, ranks[0]/32.0)
			fileMatch.addScore("file-rank", weight*normalized, opts.DebugScore)
		}
	}

	md := d.repoMetaData[d.repos[doc]]
	fileMatch.addScore("doc-order", scoreFileOrderFactor*(1.0-float64(doc)/float64(len(d.boundaries))), opts.DebugScore)
	fileMatch.addScore("repo-rank", scoreRepoRankFactor*float64(md.Rank)/maxUInt16, opts.DebugScore)
}

// scoreFileUsingBM25 computes a score for the file match using an approximation to BM25, the most common scoring
// algorithm for keyword search: https://en.wikipedia.org/wiki/Okapi_BM25. It implements all parts of the formula
// except inverse document frequency (idf), since we don't have access to global term frequency statistics.
//
// This scoring strategy ignores all other signals including document ranks. This keeps things simple for now,
// since BM25 is not normalized and can be tricky to combine with other scoring signals.
func (d *indexData) scoreFileUsingBM25(fileMatch *FileMatch, doc uint32, cands []*candidateMatch, opts *SearchOptions) {
	// Treat each candidate match as a term and compute the frequencies. For now, ignore case
	// sensitivity and treat filenames and symbols the same as content.
	termFreqs := map[string]int{}
	for _, cand := range cands {
		term := string(cand.substrLowered)
		termFreqs[term]++
	}

	// Compute the file length ratio. Usually the calculation would be based on terms, but using
	// bytes should work fine, as we're just computing a ratio.
	fileLength := float64(d.boundaries[doc+1] - d.boundaries[doc])
	numFiles := len(d.boundaries)
	averageFileLength := float64(d.boundaries[numFiles - 1]) / float64(numFiles)
	L := fileLength / averageFileLength

	// Use standard parameter defaults (used in Lucene and academic papers)
	k, b := 1.2, 0.75
	sumTf := 0.0 // Just for debugging
	score := 0.0
	for _, freq := range termFreqs {
		tf := float64(freq)
		sumTf += tf
		score += ((k + 1.0) * tf) / (k * (1.0 - b + b * L) + tf)
	}

	fileMatch.addKeywordScore(score, sumTf, L, opts.DebugScore)
}

func addRepo(res *SearchResult, repo *Repository) {
	if res.RepoURLs == nil {
		res.RepoURLs = map[string]string{}
	}
	res.RepoURLs[repo.Name] = repo.FileURLTemplate

	if res.LineFragments == nil {
		res.LineFragments = map[string]string{}
	}
	res.LineFragments[repo.Name] = repo.LineFragmentTemplate
}

type sortByOffsetSlice []*candidateMatch

func (m sortByOffsetSlice) Len() int      { return len(m) }
func (m sortByOffsetSlice) Swap(i, j int) { m[i], m[j] = m[j], m[i] }
func (m sortByOffsetSlice) Less(i, j int) bool {
	return m[i].byteOffset < m[j].byteOffset
}

// Gather matches from this document. This never returns a mixture of
// filename/content matches: if there are content matches, all
// filename matches are trimmed from the result. The matches are
// returned in document order and are non-overlapping.
//
// If `merge` is set, overlapping and adjacent matches will be merged
// into a single match. Otherwise, overlapping matches will be removed,
// but adjacent matches will remain.
func gatherMatches(mt matchTree, known map[matchTree]bool, merge bool) []*candidateMatch {
	var cands []*candidateMatch
	visitMatches(mt, known, func(mt matchTree) {
		if smt, ok := mt.(*substrMatchTree); ok {
			cands = append(cands, smt.current...)
		}
		if rmt, ok := mt.(*regexpMatchTree); ok {
			cands = append(cands, rmt.found...)
		}
		if rmt, ok := mt.(*wordMatchTree); ok {
			cands = append(cands, rmt.found...)
		}
		if smt, ok := mt.(*symbolRegexpMatchTree); ok {
			cands = append(cands, smt.found...)
		}
	})

	foundContentMatch := false
	for _, c := range cands {
		if !c.fileName {
			foundContentMatch = true
			break
		}
	}

	res := cands[:0]
	for _, c := range cands {
		if !foundContentMatch || !c.fileName {
			res = append(res, c)
		}
	}
	cands = res

	if merge {
		// Merge adjacent candidates. This guarantees that the matches
		// are non-overlapping.
		sort.Sort((sortByOffsetSlice)(cands))
		res = cands[:0]
		for i, c := range cands {
			if i == 0 {
				res = append(res, c)
				continue
			}
			last := res[len(res)-1]
			lastEnd := last.byteOffset + last.byteMatchSz
			end := c.byteOffset + c.byteMatchSz
			if lastEnd >= c.byteOffset {
				if end > lastEnd {
					last.byteMatchSz = end - last.byteOffset
				}
				continue
			}

			res = append(res, c)
		}
	} else {
		// Remove overlapping candidates. This guarantees that the matches
		// are non-overlapping, but also preserves expected match counts.
		sort.Sort((sortByOffsetSlice)(cands))
		res = cands[:0]
		for i, c := range cands {
			if i == 0 {
				res = append(res, c)
				continue
			}
			last := res[len(res)-1]
			lastEnd := last.byteOffset + last.byteMatchSz
			if lastEnd > c.byteOffset {
				continue
			}

			res = append(res, c)
		}
	}

	return res
}

func (d *indexData) branchIndex(docID uint32) int {
	mask := d.fileBranchMasks[docID]
	idx := 0
	for mask != 0 {
		if mask&0x1 != 0 {
			return idx
		}
		idx++
		mask >>= 1
	}
	return -1
}

// gatherBranches returns a list of branch names taking into account any branch
// filters in the query. If the query contains a branch filter, it returns all
// branches containing the docID and matching the branch filter. Otherwise, it
// returns all branches containing docID.
func (d *indexData) gatherBranches(docID uint32, mt matchTree, known map[matchTree]bool) []string {
	var mask uint64
	visitMatches(mt, known, func(mt matchTree) {
		bq, ok := mt.(*branchQueryMatchTree)
		if !ok {
			return
		}

		mask = mask | bq.branchMask()
	})

	if mask == 0 {
		mask = d.fileBranchMasks[docID]
	}

	var branches []string
	id := uint32(1)
	branchNames := d.branchNames[d.repos[docID]]
	for mask != 0 {
		if mask&0x1 != 0 {
			branches = append(branches, branchNames[uint(id)])
		}
		id <<= 1
		mask >>= 1
	}

	return branches
}

func (d *indexData) List(ctx context.Context, q query.Q, opts *ListOptions) (rl *RepoList, err error) {
	var include func(rle *RepoListEntry) bool

	q = d.simplify(q)
	if c, ok := q.(*query.Const); ok {
		if !c.Value {
			return &RepoList{}, nil
		}
		include = func(rle *RepoListEntry) bool {
			return true
		}
	} else {
		sr, err := d.Search(ctx, q, &SearchOptions{
			ShardRepoMaxMatchCount: 1,
		})
		if err != nil {
			return nil, err
		}

		foundRepos := make(map[string]struct{}, len(sr.Files))
		for _, file := range sr.Files {
			foundRepos[file.Repository] = struct{}{}
		}

		include = func(rle *RepoListEntry) bool {
			_, ok := foundRepos[rle.Repository.Name]
			return ok
		}
	}

	var l RepoList

	field, err := opts.GetField()
	if err != nil {
		return nil, err
	}
	switch field {
	case RepoListFieldRepos:
		l.Repos = make([]*RepoListEntry, 0, len(d.repoListEntry))
	case RepoListFieldMinimal:
		l.Minimal = make(map[uint32]*MinimalRepoListEntry, len(d.repoListEntry))
	case RepoListFieldReposMap:
		l.ReposMap = make(ReposMap, len(d.repoListEntry))
	}

	for i := range d.repoListEntry {
		if d.repoMetaData[i].Tombstone {
			continue
		}
		rle := &d.repoListEntry[i]
		if !include(rle) {
			continue
		}

		l.Stats.Add(&rle.Stats)

		// Backwards compat for when ID is missing
		if rle.Repository.ID == 0 {
			l.Repos = append(l.Repos, rle)
			continue
		}

		switch field {
		case RepoListFieldRepos:
			l.Repos = append(l.Repos, rle)
		case RepoListFieldMinimal:
			l.Minimal[rle.Repository.ID] = &MinimalRepoListEntry{
				HasSymbols:    rle.Repository.HasSymbols,
				Branches:      rle.Repository.Branches,
				IndexTimeUnix: rle.IndexMetadata.IndexTime.Unix(),
			}
		case RepoListFieldReposMap:
			l.ReposMap[rle.Repository.ID] = MinimalRepoListEntry{
				HasSymbols:    rle.Repository.HasSymbols,
				Branches:      rle.Repository.Branches,
				IndexTimeUnix: rle.IndexMetadata.IndexTime.Unix(),
			}
		}

	}

	// Only one of these fields is populated and in all cases the size of that
	// field is the number of Repos in this shard.
	l.Stats.Repos = len(l.Repos) + len(l.Minimal) + len(l.ReposMap)

	return &l, nil
}

// regexpToMatchTreeRecursive converts a regular expression to a matchTree mt. If
// mt is equivalent to the input r, isEqual = true and the matchTree can be used
// in place of the regex r. If singleLine = true, then the matchTree and all
// its children only match terms on the same line. singleLine is used during
// recursion to decide whether to return an andLineMatchTree (singleLine = true)
// or a andMatchTree (singleLine = false).
func (d *indexData) regexpToMatchTreeRecursive(r *syntax.Regexp, minTextSize int, fileName bool, caseSensitive bool) (mt matchTree, isEqual bool, singleLine bool, err error) {
	// TODO - we could perhaps transform Begin/EndText in '\n'?
	// TODO - we could perhaps transform CharClass in (OrQuery )
	// if there are just a few runes, and part of a OpConcat?
	switch r.Op {
	case syntax.OpLiteral:
		s := string(r.Rune)
		if len(s) >= minTextSize {
			mt, err := d.newSubstringMatchTree(&query.Substring{Pattern: s, FileName: fileName, CaseSensitive: caseSensitive})
			return mt, true, !strings.Contains(s, "\n"), err
		}
	case syntax.OpCapture:
		return d.regexpToMatchTreeRecursive(r.Sub[0], minTextSize, fileName, caseSensitive)

	case syntax.OpPlus:
		return d.regexpToMatchTreeRecursive(r.Sub[0], minTextSize, fileName, caseSensitive)

	case syntax.OpRepeat:
		if r.Min == 1 {
			return d.regexpToMatchTreeRecursive(r.Sub[0], minTextSize, fileName, caseSensitive)
		} else if r.Min > 1 {
			// (x){2,} can't be expressed precisely by the matchTree
			mt, _, singleLine, err := d.regexpToMatchTreeRecursive(r.Sub[0], minTextSize, fileName, caseSensitive)
			return mt, false, singleLine, err
		}
	case syntax.OpConcat, syntax.OpAlternate:
		var qs []matchTree
		isEq := true
		singleLine = true
		for _, sr := range r.Sub {
			if sq, subIsEq, subSingleLine, err := d.regexpToMatchTreeRecursive(sr, minTextSize, fileName, caseSensitive); sq != nil {
				if err != nil {
					return nil, false, false, err
				}
				isEq = isEq && subIsEq
				singleLine = singleLine && subSingleLine
				qs = append(qs, sq)
			}
		}
		if r.Op == syntax.OpConcat {
			if len(qs) > 1 {
				isEq = false
			}
			newQs := make([]matchTree, 0, len(qs))
			for _, q := range qs {
				if _, ok := q.(*bruteForceMatchTree); ok {
					continue
				}
				newQs = append(newQs, q)
			}
			if len(newQs) == 1 {
				return newQs[0], isEq, singleLine, nil
			}
			if len(newQs) == 0 {
				return &bruteForceMatchTree{}, isEq, singleLine, nil
			}
			if singleLine {
				return &andLineMatchTree{andMatchTree{children: newQs}}, isEq, singleLine, nil
			}
			return &andMatchTree{newQs}, isEq, singleLine, nil
		}
		for _, q := range qs {
			if _, ok := q.(*bruteForceMatchTree); ok {
				return q, isEq, false, nil
			}
		}
		if len(qs) == 0 {
			return &noMatchTree{Why: "const"}, isEq, false, nil
		}
		return &orMatchTree{qs}, isEq, false, nil
	case syntax.OpStar:
		if r.Sub[0].Op == syntax.OpAnyCharNotNL {
			return &bruteForceMatchTree{}, false, true, nil
		}
	}
	return &bruteForceMatchTree{}, false, false, nil
}
