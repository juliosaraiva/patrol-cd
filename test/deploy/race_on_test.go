//go:build race

package deploy_test

// raceEnabled reports that the test binary was built with -race (the patrolcd binary run by
// the RBAC tests is then built with -race too).
const raceEnabled = true
