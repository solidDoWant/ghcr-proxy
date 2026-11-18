#!/bin/sh
# Prints the files a pull request run's checkout changes against master, one
# per line. HEAD is the PR head, or the PR head merged with master when that
# merge created a commit; either way the merge base with origin/master is the
# commit the changes start from.
set -eu
git diff --name-only "$(git merge-base HEAD origin/master)" HEAD
