import { SectionHeading } from '@/components/section-heading';
import { repoFile } from '@/lib/site';

const rows = [
    {
        topic: 'Compute',
        rented: 'Every job draws on metered runner minutes.',
        owned: 'Jobs run on Macs and Linux machines you already own.'
    },
    {
        topic: 'Caches and artifacts',
        rented: 'Hosted cache and artifact storage quotas, reached across the WAN.',
        owned: 'A cache gate and artifact store on your LAN, limited by your own disks and retention.'
    },
    {
        topic: 'Machine shapes',
        rented: "The provider's catalogue of sizes.",
        owned: 'Profiles you declare as exact CPU and RAM vectors, named in the label.'
    },
    {
        topic: 'Control',
        rented: 'Capacity and placement decided elsewhere.',
        owned: 'Admission, priority and updates you can inspect through one CLI.'
    }
];

export const Why = () => (
    <section aria-labelledby="why" className="border-t border-line/60 bg-bg-raised/40 py-20 sm:py-28">
        <div className="mx-auto max-w-6xl px-4 sm:px-6">
            <SectionHeading id="why" index="03" eyebrow="Why" title="Rented minutes, or hardware you already own.">
                GitHub still provides workflow orchestration. Unyoke moves execution, caches and artifacts onto machines under your control.
            </SectionHeading>

            <div className="reveal mt-16 overflow-hidden rounded-2xl border border-line">
                <table className="block w-full border-collapse text-left text-sm md:table">
                    <caption className="sr-only">Hosted runners compared with Unyoke on your own hardware</caption>
                    <thead className="hidden bg-surface md:table-header-group">
                        <tr>
                            <th
                                scope="col"
                                className="w-1/5 px-6 py-4 font-mono text-xs font-normal tracking-wider text-fg-subtle uppercase"
                            >
                                <span className="sr-only">Topic</span>
                            </th>
                            <th scope="col" className="px-6 py-4 font-mono text-xs font-normal tracking-wider text-fg-subtle uppercase">
                                Rented runners
                            </th>
                            <th scope="col" className="px-6 py-4 font-mono text-xs font-normal tracking-wider text-accent-text uppercase">
                                Unyoke on your hardware
                            </th>
                        </tr>
                    </thead>
                    <tbody className="block md:table-row-group">
                        {rows.map(row => (
                            <tr key={row.topic} className="grid border-t border-line first:border-t-0 md:table-row md:first:border-t">
                                <th scope="row" className="px-6 pt-5 font-semibold md:py-5 md:align-top">
                                    {row.topic}
                                </th>
                                <td className="px-6 pt-2 text-fg-muted md:py-5 md:align-top">
                                    <span className="mr-2 font-mono text-[11px] text-fg-subtle uppercase md:hidden">Rented</span>
                                    {row.rented}
                                </td>
                                <td className="bg-accent-soft px-6 py-3 md:py-5 md:align-top">
                                    <span className="mr-2 font-mono text-[11px] text-accent-text uppercase md:hidden">Unyoke</span>
                                    {row.owned}
                                </td>
                            </tr>
                        ))}
                    </tbody>
                </table>
            </div>

            <p className="reveal mx-auto mt-10 max-w-3xl text-center text-sm leading-relaxed text-fg-muted">
                Unyoke is local infrastructure for GitHub Actions, not a replacement for GitHub, and owned hardware still has capacity,
                power and upkeep. Its own CI runs on runners the fleet schedules for itself, under a{' '}
                <a href={`${repoFile('README.md')}#development`} className="text-accent-text underline underline-offset-4">
                    two-generation rule
                </a>
                .
            </p>
        </div>
    </section>
);
