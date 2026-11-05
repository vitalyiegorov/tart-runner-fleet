package app

import (
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// imageSignals names what an operator should read about each image generation (ADR 0094).
// It is the end of the line: nothing in the fleet reads the result to decide anything, so
// a signal that is wrong costs a sentence, which is why it may be cheap where a hold may
// not. It sits last in the tick, after the plan, to keep that plain.
func imageSignals(queued []domain.Demand, exits map[domain.EmptyExitKey]domain.EmptyExits,
	readings map[domain.Platform]domain.RunnerRefusal, images map[domain.Platform]string, now time.Time,
) []ImageSignal {
	type tally struct {
		exits, demands int
		last, assigned time.Time
	}
	tallies := map[domain.Platform]*tally{}
	for _, demand := range queued {
		spent := exits[domain.EmptyExitKey{Demand: demand.Key, Image: images[demand.Platform]}]
		if spent.Unassigned == 0 || spent.Last.After(now) {
			continue
		}
		current := tallies[demand.Platform]
		if current == nil {
			current = &tally{}
			tallies[demand.Platform] = current
		}
		current.exits += spent.Unassigned
		current.demands++
		if spent.Last.After(current.last) {
			current.last = spent.Last
		}
		if spent.Assigned.After(current.assigned) {
			current.assigned = spent.Assigned
		}
	}
	var signals []ImageSignal
	for _, platform := range []domain.Platform{domain.PlatformLinux, domain.PlatformMacOS} {
		image := images[platform]
		if image == "" {
			image = string(platform)
		}
		if current := tallies[platform]; current != nil && current.exits >= domain.ImageSignalMinExits &&
			now.Sub(current.last) <= domain.ImageSignalWindow && now.Sub(current.assigned) >= domain.ImageSignalWindow {
			since := "in the history kept"
			if !current.assigned.IsZero() {
				since = "since " + current.assigned.UTC().Format(time.RFC3339)
			}
			signals = append(signals, ImageSignal{Name: domain.SignalImageNotReceivingJobs, Image: image,
				Detail: fmt.Sprintf("%s: %s: %d instances across %d queued jobs exited without a job, the last at %s, and no job "+
					"has been assigned on this generation %s. A refused runner is one cause; dispatch elsewhere can also cause it. "+
					"This changes no launch decisions.", domain.SignalImageNotReceivingJobs, image, current.exits, current.demands,
					current.last.UTC().Format(time.RFC3339), since)})
		}
		if reading, read := readings[platform]; read && !reading.At.After(now) && now.Sub(reading.At) <= domain.ImageSignalWindow {
			signals = append(signals, ImageSignal{Name: domain.SignalRunnerVersionRefused, Image: image,
				Detail: fmt.Sprintf("%s (advisory: a job can write this log, and it changes no launch decisions): %s: guest %s, which "+
					"took no job as far as the fleet knows, logged at %s that GitHub refuses actions/runner %s as deprecated",
					domain.SignalRunnerVersionRefused, image, reading.Instance, reading.At.UTC().Format(time.RFC3339), reading.Version)})
		}
	}
	return signals
}
