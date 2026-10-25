package factory

import (
	"context"
	"errors"
	"maps"
	"strings"
)

// gitStub answers f.GitOut from canned output keyed by the joined git args (first map wins); other
// calls fail.
func gitStub(answers ...map[string]string) func(context.Context, string, ...string) (string, error) {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		k := strings.Join(args, " ")
		for _, m := range answers {
			if out, ok := m[k]; ok {
				return out, nil
			}
		}
		return "", errors.New("unexpected git " + k)
	}
}

// directOK: a linked worktree whose only own commit touched its work item.
var directOK = map[string]string{
	"rev-parse --path-format=absolute --git-dir --git-common-dir": "/r/.git/worktrees/c1\n/r/.git\n",
	"reflog show --format=%H%x09%gs HEAD":                         "bbb\trebase (finish): returning\nbbb\trebase (pick): note\nm1\trebase (start): checkout origin/main\naaa\tcommit: note\n",
	"show --name-only --format= aaa":                              "work/2026-10-25-x/README.md\n",
}

func withAnswers(m map[string]string, kv ...string) map[string]string {
	out := maps.Clone(m)
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}
