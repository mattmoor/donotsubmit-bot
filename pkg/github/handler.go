package github

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"sourcegraph.com/sourcegraph/go-diff/diff"

	"github.com/google/go-github/github"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	payload, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("ERROR: no payload: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// TODO(mattmoor): This should be:
	//     eventType := github.WebHookType(r)
	// https://github.com/knative/eventing-sources/issues/120
	// HACK HACK HACK
	eventType := strings.Split(r.Header.Get("ce-eventtype"), ".")[4]

	event, err := github.ParseWebHook(eventType, payload)
	if err != nil {
		log.Printf("ERROR: unable to parse webhook: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The set of events here should line up with what is in
	//   config/one-time/github-source.yaml
	switch event := event.(type) {
	case *github.PullRequestEvent:
		if err := HandlePullRequest(event); err != nil {
			log.Printf("Error handling %T: %v", event, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	default:
		log.Printf("Unrecognized event: %T", event)
		http.Error(w, "Unknown event", http.StatusBadRequest)
		return
	}
}

// TODO(mattmoor): For bonus points, return the position to comment on.
func HasDoNotSubmit(cf *github.CommitFile) bool {
	hs, err := diff.ParseHunks([]byte(cf.GetPatch()))
	if err != nil {
		log.Printf("ERROR PARSING HUNKS: %v", err)
		return false
	}

	// Search the lines of each diff "hunk" for an addition line containing
	// the words "DO NOT SUBMIT".
	for _, hunk := range hs {
		s := string(hunk.Body)
		lines := strings.Split(s, "\n")
		for _, line := range lines {
			if !strings.HasPrefix(line, "+") {
				continue
			}
			if strings.Contains(line, "DO NOT SUBMIT") {
				return true
			}
		}
	}

	return false
}

func HasLabel(pr *github.PullRequest, label string) bool {
	for _, l := range pr.Labels {
		if l.GetName() == label {
			return true
		}
	}
	return false
}

// Determine whether we need a `/hold` on this PR.
func NeedsHold(ctx context.Context, pre *github.PullRequestEvent) (bool, error) {
	ghc := GetClient(ctx)

	owner, repo := pre.Repo.Owner.GetLogin(), pre.Repo.GetName()

	lopt := &github.ListOptions{}
	for {
		cfs, resp, err := ghc.PullRequests.ListFiles(ctx, owner, repo, pre.GetNumber(), lopt)
		if err != nil {
			return false, err
		}
		for _, cf := range cfs {
			if HasDoNotSubmit(cf) {
				return true, nil
			}
		}
		if lopt.Page == resp.NextPage {
			break
		}
		lopt.Page = resp.NextPage
	}

	return false, nil
}

func HandlePullRequest(pre *github.PullRequestEvent) error {
	pr := pre.GetPullRequest()
	log.Printf("PR: %v", pr.String())

	// Ignore closed PRs
	if pr.GetState() == "closed" {
		return nil
	}
	ctx := context.Background()
	ghc := GetClient(ctx)

	want, err := NeedsHold(ctx, pre)
	if err != nil {
		return err
	}

	holdLabel := "do-not-merge/hold"
	owner, repo := pre.Repo.Owner.GetLogin(), pre.Repo.GetName()

	got := HasLabel(pr, holdLabel)

	// Want, but don't have.
	if want && !got {
		// Add the label
		_, _, err = ghc.Issues.AddLabelsToIssue(ctx, owner, repo, pr.GetNumber(),
			[]string{holdLabel})
		return err
	}

	// Have, but don't want.
	// TODO(mattmoor): We probably don't want to do this because there isn't a good way
	// to determine who put the hold on the PR (it might not have been us!)
	if !want && got {
		_, err = ghc.Issues.RemoveLabelForIssue(ctx, owner, repo, pr.GetNumber(),
			holdLabel)
		return err
	}

	return nil
}
