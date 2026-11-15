import { ArrowIcon } from '@/components/icons';
import { SectionHeading } from '@/components/section-heading';
import { repoFile, site } from '@/lib/site';

const steps = [
    {
        title: 'Install a verified release',
        body: 'Download the release assets, check them against SHA256SUMS, and extract them into a versioned release directory.',
        code: 'shasum -a 256 -c SHA256SUMS'
    },
    {
        title: 'Configure',
        body: 'Start from the example configuration, add your scoped GitHub block, and validate it without starting the daemon.',
        code: 'fleet config validate fleet.json'
    },
    {
        title: 'Provision scale sets',
        body: 'Plan first. Applying needs --apply --write, an exact confirmation token and a reason.',
        code: 'fleet scale-sets provision --config fleet.json'
    },
    {
        title: 'Verify',
        body: 'Read the status, run the doctor, and require readiness before you route work to the node.',
        code: 'fleet status --require-ready -o json'
    }
];

const guides = [
    { label: 'macOS install', href: repoFile('INSTALL.md') },
    { label: 'Linux install', href: repoFile('INSTALL-linux.md') },
    { label: 'Usage', href: repoFile('USAGE.md') },
    { label: 'CLI reference', href: repoFile('docs/CLI.md') },
    { label: 'Operations', href: repoFile('docs/OPERATIONS.md') },
    { label: 'Security', href: repoFile('docs/SECURITY.md') }
];

export const QuickStart = () => (
    <section aria-labelledby="quick-start" className="py-20 sm:py-28">
        <div className="mx-auto max-w-6xl px-4 sm:px-6">
            <SectionHeading id="quick-start" index="04" eyebrow="Quick start" title="Four steps, each one checkable.">
                The README carries the full sequence, prerequisites and a way to try the binary in observe mode without touching a real
                fleet.
            </SectionHeading>

            <ol className="mt-16 grid grid-cols-1 gap-4 md:grid-cols-2">
                {steps.map((step, index) => (
                    <li key={step.title} className="reveal card flex flex-col gap-3 p-6">
                        <p className="font-mono text-xs text-accent-text">Step {index + 1}</p>
                        <h3 className="text-lg font-semibold">{step.title}</h3>
                        <p className="text-sm leading-relaxed text-fg-muted">{step.body}</p>
                        <pre
                            className="mt-auto overflow-x-auto rounded-lg border border-line bg-bg-raised px-4 py-3 text-[13px]"
                            tabIndex={0}
                        >
                            <code>{step.code}</code>
                        </pre>
                    </li>
                ))}
            </ol>

            <div className="reveal mt-10 flex flex-col items-center gap-6">
                <a
                    href={`${site.repository}#quickstart`}
                    className="group inline-flex items-center gap-2 rounded-xl bg-accent-solid px-5 py-3 font-medium text-accent-on transition hover:brightness-110"
                >
                    Follow the quick start
                    <ArrowIcon className="size-4 transition-transform group-hover:translate-x-0.5" />
                </a>
                <nav aria-label="Guides">
                    <ul className="flex flex-wrap justify-center gap-2 text-sm">
                        {guides.map(guide => (
                            <li key={guide.href}>
                                <a
                                    href={guide.href}
                                    className="inline-block rounded-lg border border-line px-3 py-1.5 text-fg-muted transition-colors hover:border-accent hover:text-fg"
                                >
                                    {guide.label}
                                </a>
                            </li>
                        ))}
                    </ul>
                </nav>
            </div>
        </div>
    </section>
);
