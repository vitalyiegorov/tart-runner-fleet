import { Mark } from '@/components/brand';
import { ArrowIcon, BookIcon, GitHubIcon } from '@/components/icons';
import { site } from '@/lib/site';

const commands = [
    { command: 'fleet status --require-ready', note: 'controller, host pressure, queues, instances' },
    { command: 'fleet queues -o json', note: 'queued jobs and oldest age, per profile and scope' },
    { command: 'fleet instances', note: 'live VMs, vCPU and memory by profile' },
    { command: 'fleet doctor', note: 'API, live, ready and metrics checks' }
];

const Terminal = () => (
    <figure className="card relative overflow-hidden shadow-2xl shadow-black/20" aria-labelledby="terminal-caption">
        <div className="flex items-center gap-2 border-b border-line px-4 py-3">
            <span className="size-2.5 rounded-full bg-line-strong" aria-hidden="true" />
            <span className="size-2.5 rounded-full bg-line-strong" aria-hidden="true" />
            <span className="size-2.5 rounded-full bg-line-strong" aria-hidden="true" />
            <figcaption id="terminal-caption" className="ml-2 font-mono text-xs text-fg-subtle">
                read-only, over a 0600 Unix socket
            </figcaption>
        </div>
        <pre className="px-4 py-5 text-[13px] leading-7 whitespace-pre-wrap sm:px-5">
            <code>
                {commands.map(({ command, note }) => (
                    <span key={command} className="block">
                        <span className="text-accent-text select-none" aria-hidden="true">
                            ${' '}
                        </span>
                        <span className="text-fg">{command}</span>
                        <span className="block text-fg-subtle"># {note}</span>
                    </span>
                ))}
                <span className="block text-fg-subtle">
                    # <span className="text-fg-muted">-o json</span> speaks the versioned <span className="text-accent-text">fleet.v1</span>{' '}
                    API
                </span>
            </code>
        </pre>
    </figure>
);

export const Hero = () => (
    <section id="top" aria-labelledby="hero-title" className="relative isolate overflow-hidden">
        <div className="bg-grid absolute inset-0 -z-10" aria-hidden="true" />
        <div className="glow absolute -top-40 left-1/2 -z-10 h-[36rem] w-[60rem] -translate-x-1/2" aria-hidden="true" />
        <div className="mx-auto grid max-w-6xl items-center gap-12 px-4 pt-16 pb-20 sm:px-6 sm:pt-24 grid-cols-1 lg:grid-cols-[minmax(0,1.1fr)_minmax(0,1fr)] lg:gap-16 lg:pt-28 lg:pb-28">
            <div className="motion-rise">
                <Mark className="size-16 sm:size-20" animated />
                <p className="eyebrow mt-8">Formerly Tart Runner Fleet · AGPL-3.0</p>
                <h1
                    id="hero-title"
                    className="mt-4 text-5xl leading-[1.02] font-semibold tracking-tighter text-balance sm:text-6xl lg:text-7xl"
                >
                    CI/CD, on your <span className="text-accent-text">own terms.</span>
                </h1>
                <p className="mt-6 max-w-xl text-lg leading-relaxed text-pretty text-fg-muted sm:text-xl">
                    Unyoke runs GitHub Actions jobs on Macs and Linux machines you own: a fresh VM or container per job, admitted within
                    exact resource budgets, with caches and artifacts kept on your LAN.
                </p>
                <div className="mt-9 flex flex-col gap-3 sm:flex-row">
                    <a
                        href={site.repository}
                        className="group inline-flex items-center justify-center gap-2 rounded-xl bg-accent-solid px-5 py-3 font-medium text-accent-on shadow-lg shadow-accent/20 transition hover:brightness-110"
                    >
                        <GitHubIcon />
                        View on GitHub
                        <ArrowIcon className="size-4 transition-transform group-hover:translate-x-0.5" />
                    </a>
                    <a
                        href={site.docs}
                        className="inline-flex items-center justify-center gap-2 rounded-xl border border-line-strong px-5 py-3 font-medium transition-colors hover:border-accent hover:text-accent-text"
                    >
                        <BookIcon className="size-5" />
                        Read the docs
                    </a>
                </div>
            </div>
            <div className="motion-rise [animation-delay:150ms]">
                <Terminal />
            </div>
        </div>
    </section>
);
