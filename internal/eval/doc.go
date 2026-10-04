// Package eval scores an evaluation replay of magnum's pull request review against a corpus of
// pull requests with known ("seeded") defects.
//
// The corpus (corpus.go) lists, per pull request, the reviewed head and the defects a good review
// must report. The judge's result file of a no-post run (findings.go) is parsed into findings, and
// ScoreCase (score.go) tells which defects were found, at which severity, and how many findings
// matched nothing (noise). Runs (report.go) collect the scores of one replay, persist them, can be
// rescored after the corpus is corrected, and render a text or markdown report.
//
// The package is pure: it starts no process, touches no network and knows nothing of the registry.
// Its only file access is the corpus file and the run directories it is told about.
package eval
