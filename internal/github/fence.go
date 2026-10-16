package github

import (
	"context"
	"errors"
	"fmt"
)

// HeadReader reads a PR's current head commit. *Client implements it.
type HeadReader interface {
	HeadSHA(ctx context.Context, repo Repo, number int) (string, error)
}

// ErrHeadMoved means the PR's head is no longer the commit the pass reviewed,
// so a newer run owns the PR and this one must stop publishing (design §2).
var ErrHeadMoved = errors.New("pull request head moved")

// HeadMovedError reports which head the pass reviewed and which it found.
type HeadMovedError struct {
	Reviewed string
	Current  string
}

func (e *HeadMovedError) Error() string {
	return fmt.Sprintf("pull request head moved from %s to %s", short(e.Reviewed), short(e.Current))
}

// Is lets errors.Is(err, ErrHeadMoved) match.
func (e *HeadMovedError) Is(target error) bool { return target == ErrHeadMoved }

// Fence pins publishing to the head a pass reviewed. Call Check before
// publishing and again before each state-changing mutation (resolve,
// unresolve, transition reply). The fence is an optimization against
// pointless mutations; correctness comes from the replay rules (design §6).
type Fence struct {
	heads  HeadReader
	repo   Repo
	sha    string
	number int
}

// NewFence returns a fence for the pass that reviewed sha.
func NewFence(heads HeadReader, repo Repo, number int, sha string) *Fence {
	return &Fence{heads: heads, repo: repo, number: number, sha: sha}
}

// SHA is the head the pass reviewed.
func (f *Fence) SHA() string { return f.sha }

// Check returns a *HeadMovedError if the PR's head is no longer the reviewed
// one, or the error from reading it.
func (f *Fence) Check(ctx context.Context) error {
	cur, err := f.heads.HeadSHA(ctx, f.repo, f.number)
	if err != nil {
		return fmt.Errorf("fence: %w", err)
	}
	if cur != f.sha {
		return &HeadMovedError{Reviewed: f.sha, Current: cur}
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
