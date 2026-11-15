import { Features } from '@/components/features';
import { Hero } from '@/components/hero';
import { HowItWorks } from '@/components/how-it-works';
import { QuickStart } from '@/components/quick-start';
import { SiteFooter } from '@/components/site-footer';
import { SiteHeader } from '@/components/site-header';
import { Why } from '@/components/why';

const HomePage = () => (
    <>
        <SiteHeader />
        <main id="main" tabIndex={-1} className="outline-none">
            <Hero />
            <HowItWorks />
            <Features />
            <Why />
            <QuickStart />
        </main>
        <SiteFooter />
    </>
);

export default HomePage;
