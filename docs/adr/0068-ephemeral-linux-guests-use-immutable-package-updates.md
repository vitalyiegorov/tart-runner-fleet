# 0068: Ephemeral Linux guests use immutable package updates

Status: accepted.

## Context

[Issue #148](https://github.com/vitalyiegorov/tart-runner-fleet/issues/148)
records unattended upgrades holding the dpkg lock while a CI job installs
Playwright dependencies. A Tart guest lives for one job and inherits its
packages from a sealed base; background updates introduce contention and make
that package set depend on boot timing.

## Decision

Update packages during operator-controlled base-image maintenance, then seal
and verify the replacement. Do not update ephemeral Tart guests automatically.
The [Linux base-image recipe](../LINUX_BASE_IMAGE.md#4c-stop-apt-from-upgrading-the-guest-during-a-job)
masks and stops `unattended-upgrades.service`, `apt-daily.timer`,
`apt-daily-upgrade.timer`, `apt-daily.service`, and `apt-daily-upgrade.service`,
and disables APT periodic package-list and unattended-upgrade work.

All five units must be masked and inactive immediately after the change and
after reboot. Stop errors, unavailable observations, and unexpected states
abort verification; masking without stopping is insufficient. Read systemd
properties so an inactive unit is accepted without ignoring command failures.
Containers without systemd do not need this timer policy.

## Consequences and limits

Security updates require regular base-image rebuilds and explicit rollout;
automatic guest updates are not a substitute. Keep the previous base for
rollback, consistent with ADR 0010. Jobs may still install required packages;
this policy removes the image's background updater as a competing lock holder.
It does not claim that every possible dpkg lock holder is absent.

Documentation only: no controller, API, schema or host-service change. The
fleet simulation does not model guest systemd or apt; the masked/inactive checks
on a rebooted candidate are the rollout evidence.
