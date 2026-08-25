package controlplane

import (
	"time"

	"github.com/open-cluster/oc-control-plane/test/eval"
)

type causeTruth = eval.Cause
type readTruth = eval.Read
type groundTruth = eval.GroundTruth
type evalCase = eval.Case
type evalMessage = eval.Message
type evalChannel = eval.Channel
type evalWorkspace = eval.Workspace
type evalPull = eval.Pull
type evalRepo = eval.Repository
type evalInstallation = eval.Installation

const (
	evalPrimaryToken        = eval.PrimaryToken
	evalPrimaryInstallation = eval.PrimaryInstallation
)

func evalCases(now time.Time) []evalCase {
	return eval.Cases(now)
}
