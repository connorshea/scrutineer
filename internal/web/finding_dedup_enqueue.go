package web

import (
	"context"

	"scrutineer/internal/db"
)

// findingDedupSkillName is the repository-scoped pass that compares open
// findings and marks ones describing the same vulnerability as duplicates.
// See skills/finding-dedup/SKILL.md.
const findingDedupSkillName = "finding-dedup"

// autoEnqueueFindingDedupAfterDeepDive is wired onto Worker.OnScanDone. The
// worker calls it once after a scan completes and its findings are
// committed. We enqueue a repository-scoped finding-dedup run only when both
// conditions the dedup pass needs to be worth its model spend hold:
//
//  1. The scan is a security-deep-dive that produced at least one *new*
//     finding. Re-observed findings keep the scan_id of the run that first
//     created them, so counting findings with scan_id == this scan counts
//     exactly the new rows. Nothing new means nothing fresh to dedup.
//  2. The repository already held other non-scanner findings before this
//     scan — open deep-dive/legacy/manual rows from a prior scan, an import
//     of that kind, or a manual entry. Without something else to compare
//     the new findings against, dedup has no pairs to consider.
//
// "Non-scanner" matches the Findings-tab toggle exactly (nonScannerScanFilter):
// the cheap tool scanners (semgrep, zizmor) and tool imports (CodeQL, Snyk)
// do not count. Both counts read committed state rather than threading a
// tally through the callback, so the decision is independent of parse-time
// races.
//
// Errors are logged and swallowed: failing to enqueue the dedup pass must
// never fail the upstream scan.
func (s *Server) autoEnqueueFindingDedupAfterDeepDive(scan *db.Scan) {
	if scan == nil || scan.SkillName != deepDiveSkillName {
		return
	}

	var newFromScan int64
	if err := s.DB.Model(&db.Finding{}).
		Where("scan_id = ?", scan.ID).
		Count(&newFromScan).Error; err != nil {
		s.Log.Warn("auto-enqueue finding-dedup: count new findings",
			"scan", scan.ID, "repo", scan.RepositoryID, "err", err)
		return
	}
	if newFromScan == 0 {
		return
	}

	var otherNonScanner int64
	if err := s.DB.Model(&db.Finding{}).
		Where("repository_id = ? AND scan_id <> ?", scan.RepositoryID, scan.ID).
		Where(nonScannerScanFilter, deepDiveSkillName).
		Where("status NOT IN (" + db.ClosedFindingLifecycleSQLValues() + ")").
		Count(&otherNonScanner).Error; err != nil {
		s.Log.Warn("auto-enqueue finding-dedup: count existing findings",
			"scan", scan.ID, "repo", scan.RepositoryID, "err", err)
		return
	}
	if otherNonScanner == 0 {
		return
	}

	s.enqueueFindingDedupForRepo(context.Background(), scan.RepositoryID)
}

// enqueueFindingDedupForRepo looks up the active finding-dedup skill and
// enqueues a repository-scoped run. No dedup skill registered means no
// auto-dedup, which is fine; the workflow degrades to leaving duplicates for
// a human rather than failing the upstream scan. A dedup run already queued
// or in flight for this repo is a no-op so concurrent deep-dives do not pile
// up redundant passes.
func (s *Server) enqueueFindingDedupForRepo(ctx context.Context, repoID uint) {
	var skill db.Skill
	if err := s.DB.Where("name = ? AND active = ?", findingDedupSkillName, true).First(&skill).Error; err != nil {
		return
	}
	if s.hasOpenRepoScopedScan(repoID, skill.ID) {
		return
	}
	if _, err := s.enqueueSkillWith(ctx, repoID, skill.ID, ScanOpts{}); err != nil {
		s.Log.Warn("auto-enqueue finding-dedup",
			"repo", repoID, "skill", findingDedupSkillName, "err", err)
	}
}

// hasOpenRepoScopedScan returns true when a queued or running repository-scoped
// scan (no finding attached) of the given skill already exists for the repo.
// Mirrors hasOpenFindingScopedScan for repo-wide passes like finding-dedup.
func (s *Server) hasOpenRepoScopedScan(repoID, skillID uint) bool {
	return s.hasOpenScan("repository_id = ? AND skill_id = ? AND finding_id IS NULL", repoID, skillID)
}
