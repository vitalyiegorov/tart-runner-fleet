import type { ReactNode } from 'react';

import { featureIcons } from '@/components/icons';
import { SectionHeading } from '@/components/section-heading';
import { adr, repoFile } from '@/lib/site';

interface Evidence {
    label: string;
    href: string;
}

interface Feature {
    title: string;
    icon: ReactNode;
    body: ReactNode;
    evidence: Evidence[];
    span: string;
    badge?: string;
}

const Code = ({ children }: { children: ReactNode }) => (
    <code className="rounded bg-bg-raised px-1.5 py-0.5 text-[0.85em] whitespace-nowrap text-fg">{children}</code>
);

const features: Feature[] = [
    {
        title: 'Ephemeral per-job runners',
        icon: featureIcons.ephemeral,
        body: (
            <>
                Every job gets a fresh Tart VM on Apple silicon or a Podman container on Linux, deleted when the job ends. No long-lived
                runner accumulates state or credentials.
            </>
        ),
        evidence: [
            { label: 'ADR 0010', href: adr('0010-ephemeral-guest-shutdown') },
            { label: 'ADR 0058', href: adr('0058-a-node-declares-the-execution-technology-it-has') }
        ],
        span: 'lg:col-span-3'
    },
    {
        title: 'A multi-node fleet with exact admission',
        icon: featureIcons.fleet,
        body: (
            <>
                Each node serves the scale sets it owns. A job is admitted against exact CPU, RAM and slot vectors, then a live probe of
                disk, memory, swap and CPU defers it under pressure. Labels state the shape: <Code>trf-macos-arm64-6x12</Code>.
            </>
        ),
        evidence: [
            { label: 'ADR 0034', href: adr('0034-a-node-serves-the-scale-sets-it-owns') },
            { label: 'ADR 0012', href: adr('0012-shared-cross-platform-capacity') },
            { label: 'ADR 0032', href: adr('0032-resource-explicit-runner-labels') }
        ],
        span: 'lg:col-span-3'
    },
    {
        title: 'Fail-closed by design',
        icon: featureIcons.failClosed,
        body: (
            <>
                A stale or unavailable observation never reads as &ldquo;no work&rdquo;. Destructive actions need fresh, corroborated
                evidence of ownership.
            </>
        ),
        evidence: [
            { label: 'ADR 0002', href: adr('0002-official-scale-set-protocol') },
            { label: 'ADR 0042', href: adr('0042-a-destructive-premise-must-be-corroborated') }
        ],
        span: 'lg:col-span-2'
    },
    {
        title: 'LAN cache gate',
        icon: featureIcons.cache,
        body: (
            <>
                Linux guests read the Actions cache, and opt-in npm downloads, from a gate on your network. Pull requests read the cache but
                never write it; macOS guests keep GitHub&apos;s cache.
            </>
        ),
        evidence: [
            { label: 'ADR 0078', href: adr('0078-a-node-serves-its-guests-actions-cache-pull-requests-only-read-it') },
            { label: 'ADR 0093', href: adr('0093-the-home-site-reads-npm-and-public-git-through-the-gate') }
        ],
        badge: 'opt-in',
        span: 'lg:col-span-2'
    },
    {
        title: 'Local Actions artifact store',
        icon: featureIcons.artifact,
        body: <>Run-scoped artifacts live on a gate you own, independent of hosted artifact quotas and bounded by your own disk.</>,
        evidence: [{ label: 'ADR 0097', href: adr('0097-a-gate-serves-run-scoped-actions-artifacts') }],
        span: 'lg:col-span-2',
        badge: 'opt-in'
    },
    {
        title: 'One fleet CLI, versioned JSON',
        icon: featureIcons.cli,
        body: (
            <>
                Read-only <Code>fleet</Code> commands are always safe and speak <Code>fleet.v1</Code> JSON. Mutations need an exact{' '}
                <Code>--confirm</Code> token.
            </>
        ),
        evidence: [
            { label: 'CLI', href: repoFile('docs/CLI.md') },
            { label: 'API', href: repoFile('docs/API.md') }
        ],
        span: 'lg:col-span-2'
    },
    {
        title: 'Durable SQLite state',
        icon: featureIcons.database,
        body: (
            <>Instances, demand, plans and an operation outbox live in SQLite WAL. Effects are leased and idempotent; a restart resumes.</>
        ),
        evidence: [{ label: 'ADR 0003', href: adr('0003-sqlite-outbox') }],
        span: 'lg:col-span-2'
    },
    {
        title: 'Self-updating releases',
        icon: featureIcons.update,
        body: (
            <>
                Forward-only, checksum-verified updates wait for a quiet fleet, require the new version to become ready, and roll back if
                not.
            </>
        ),
        evidence: [
            { label: 'ADR 0011', href: adr('0011-atomic-production-updates') },
            { label: 'ADR 0051', href: adr('0051-a-linux-node-updates-itself') }
        ],
        span: 'lg:col-span-2'
    },
    {
        title: 'AGPL-3.0, with commercial licences',
        icon: featureIcons.licence,
        body: (
            <>
                The source is open under AGPL-3.0. Commercial licences are available from the owner; contributions are accepted under the
                CLA.
            </>
        ),
        evidence: [
            { label: 'Licence', href: repoFile('LICENSE') },
            { label: 'CLA', href: repoFile('CLA.md') }
        ],
        span: 'md:col-span-2 lg:col-span-6'
    }
];

export const Features = () => (
    <section aria-labelledby="features" className="py-20 sm:py-28">
        <div className="mx-auto max-w-6xl px-4 sm:px-6">
            <SectionHeading id="features" index="02" eyebrow="Features" title="Built for machines you are responsible for.">
                Every claim below links to the decision record or contract that defines it.
            </SectionHeading>
            <ul className="mt-16 grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-6">
                {features.map(feature => (
                    <li
                        key={feature.title}
                        className={`reveal card group flex flex-col gap-4 p-6 transition-colors hover:border-line-strong ${feature.span}`}
                    >
                        <div className="flex items-center justify-between gap-3">
                            <span className="grid size-10 place-items-center rounded-lg bg-accent-soft text-accent-text">
                                {feature.icon}
                            </span>
                            {feature.badge === undefined ? null : (
                                <span className="rounded-full border border-line-strong px-2 py-0.5 font-mono text-[11px] text-fg-muted">
                                    {feature.badge}
                                </span>
                            )}
                        </div>
                        <h3 className="text-lg font-semibold tracking-tight">{feature.title}</h3>
                        <p className="text-sm leading-relaxed text-fg-muted">{feature.body}</p>
                        <p className="mt-auto flex flex-wrap gap-x-3 gap-y-1 pt-2 font-mono text-xs">
                            <span className="sr-only">Evidence:</span>
                            {feature.evidence.map(item => (
                                <a key={item.href} href={item.href} className="text-accent-text underline-offset-4 hover:underline">
                                    {item.label}
                                </a>
                            ))}
                        </p>
                    </li>
                ))}
            </ul>
        </div>
    </section>
);
