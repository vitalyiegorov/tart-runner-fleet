import type { ReactNode } from 'react';

import { featureIcons } from '@/components/icons';
import { SectionHeading } from '@/components/section-heading';
import { adr } from '@/lib/site';

interface Stage {
    title: string;
    body: ReactNode;
    visual: ReactNode;
    tags: string[];
}

const EphemeralVm = () => (
    <svg viewBox="0 0 48 48" className="size-9" aria-hidden="true" focusable="false">
        <rect x="4" y="8" width="40" height="28" rx="5" fill="none" stroke="var(--line-strong)" strokeDasharray="3 3" />
        <g className="motion-ephemeral">
            <rect x="9" y="13" width="30" height="18" rx="3" fill="var(--accent-soft)" stroke="var(--accent)" strokeWidth="1.5" />
            <path d="m17 19 4 3-4 3M24 25h7" fill="none" stroke="var(--accent)" strokeWidth="1.5" strokeLinecap="round" />
        </g>
        <path d="M16 42h16" stroke="var(--line-strong)" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
);

const Glyph = ({ children }: { children: ReactNode }) => (
    <span className="grid size-12 place-items-center rounded-xl border border-line-strong bg-bg-raised text-accent-text">{children}</span>
);

const stages: Stage[] = [
    {
        title: 'GitHub Actions',
        body: 'Runner scale-set sessions deliver queued jobs. Workflows, triggers and orchestration stay on GitHub.',
        visual: <Glyph>{featureIcons.cli}</Glyph>,
        tags: ['trf-linux-arm64-2x4']
    },
    {
        title: 'Your nodes',
        body: 'Each Mac or Linux machine runs fleet and serves the scale sets it owns. A job is admitted only when its CPU, RAM and slot vector fits and a fresh host-pressure probe agrees.',
        visual: <Glyph>{featureIcons.fleet}</Glyph>,
        tags: ['macOS · Tart', 'Linux · Podman']
    },
    {
        title: 'One runner, one job',
        body: 'A fresh Tart VM or Podman container registers a just-in-time, ephemeral runner bound to the job GitHub gave it.',
        visual: (
            <Glyph>
                <EphemeralVm />
            </Glyph>
        ),
        tags: ['JIT registration']
    },
    {
        title: 'Destroyed after',
        body: 'When the job ends, the runner drains and deregisters, and its VM or container is deleted. Nothing carries over to the next job.',
        visual: <Glyph>{featureIcons.update}</Glyph>,
        tags: ['no long-lived runner']
    }
];

const lifecycle = ['plan', 'clone', 'boot', 'JIT register', 'assign', 'drain', 'deregister', 'stop', 'delete'];

const Connector = () => (
    <span aria-hidden="true">
        <span className="motion-pulse-y absolute top-full left-12 block h-10 w-px bg-line-strong lg:hidden" />
        <span className="motion-pulse-x absolute top-12 left-full hidden h-px w-6 bg-line-strong lg:block" />
    </span>
);

interface LanCardProps {
    title: string;
    icon: ReactNode;
    href: string;
    adrLabel: string;
    children: ReactNode;
}

const LanCard = ({ title, icon, href, adrLabel, children }: LanCardProps) => (
    <div className="card flex flex-col gap-3 p-6">
        <h3 className="flex items-center gap-3 text-lg font-semibold">
            <span className="text-accent-text">{icon}</span>
            {title}
        </h3>
        <div className="space-y-3 text-sm leading-relaxed text-fg-muted">{children}</div>
        <a href={href} className="mt-auto self-start font-mono text-xs text-accent-text underline-offset-4 hover:underline">
            {adrLabel}
        </a>
    </div>
);

export const HowItWorks = () => (
    <section aria-labelledby="how-it-works" className="border-t border-line/60 bg-bg-raised/40 py-20 sm:py-28">
        <div className="mx-auto max-w-6xl px-4 sm:px-6">
            <SectionHeading id="how-it-works" index="01" eyebrow="How it works" title="GitHub orchestrates. Your hardware runs the job.">
                Unyoke subscribes to GitHub Actions runner scale sets, admits each queued job on a node with room for it, and gives the job
                a runner that exists for that job alone.
            </SectionHeading>

            <ol className="reveal mt-16 grid grid-cols-1 gap-10 lg:grid-cols-4 lg:gap-6">
                {stages.map((stage, index) => (
                    <li key={stage.title} className="card relative flex flex-col gap-4 p-6">
                        <div className="flex items-center justify-between">
                            {stage.visual}
                            <span className="font-mono text-xs text-fg-subtle">0{index + 1}</span>
                        </div>
                        <h3 className="text-lg font-semibold">{stage.title}</h3>
                        <p className="text-sm leading-relaxed text-fg-muted">{stage.body}</p>
                        <ul className="mt-auto flex flex-wrap gap-2" aria-label={`${stage.title} details`}>
                            {stage.tags.map(tag => (
                                <li
                                    key={tag}
                                    className="rounded-md border border-line bg-bg-raised px-2 py-1 font-mono text-[11px] whitespace-nowrap text-fg-muted"
                                >
                                    {tag}
                                </li>
                            ))}
                        </ul>
                        {index < stages.length - 1 ? <Connector /> : null}
                    </li>
                ))}
            </ol>

            <div className="reveal mt-10">
                <h3 className="text-center font-mono text-xs tracking-wider text-fg-subtle uppercase">One state machine per instance</h3>
                <ol className="motion-lifecycle mt-4 flex flex-wrap justify-center gap-2" aria-label="Runner lifecycle">
                    {lifecycle.map((step, index) => (
                        <li
                            key={step}
                            className="rounded-full border border-line px-3 py-1.5 font-mono text-xs text-fg-muted"
                            style={{ animationDelay: `${index}s` }}
                        >
                            {step}
                        </li>
                    ))}
                </ol>
            </div>

            <div className="reveal mt-16 rounded-3xl border border-dashed border-line-strong p-4 sm:p-6">
                <h3 className="px-2 pb-4 text-center font-mono text-xs tracking-wider text-fg-subtle uppercase">
                    Beside every job, on your LAN
                </h3>
                <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                    <LanCard
                        title="LAN cache gate"
                        icon={featureIcons.cache}
                        href={adr('0078-a-node-serves-its-guests-actions-cache-pull-requests-only-read-it')}
                        adrLabel="ADR 0078 · ADR 0093"
                    >
                        <p>
                            Linux guests read the Actions cache, and opt-in npm packages, through a gate on your network instead of the WAN.
                            Pull requests only read the cache; they never write it. macOS guests keep GitHub&apos;s cache.
                        </p>
                        <p className="text-fg-subtle">Public git seeds are designed and ship separately.</p>
                    </LanCard>
                    <LanCard
                        title="Artifact store"
                        icon={featureIcons.artifact}
                        href={adr('0097-a-gate-serves-run-scoped-actions-artifacts')}
                        adrLabel="ADR 0097"
                    >
                        <p>
                            Run-scoped Actions artifacts upload to a gate you own, authorised by the job&apos;s signed runtime token, so the
                            handoff between jobs does not depend on hosted artifact quota.
                        </p>
                        <p className="text-fg-subtle">Off by default. Kept up to 72 hours within a byte cap on your disk.</p>
                    </LanCard>
                </div>
            </div>
        </div>
    </section>
);
