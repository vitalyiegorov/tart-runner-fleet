import { defineConfig, devices } from '@playwright/test';

const PORT = 3210;
const isCI = process.env.CI === 'true';

// The suite runs against the production build: run `pnpm build` first.
export default defineConfig({
    testDir: './tests/e2e',
    fullyParallel: true,
    forbidOnly: isCI,
    retries: 0,
    workers: isCI ? 2 : 4,
    reporter: isCI ? [['github'], ['list']] : [['list']],
    outputDir: 'test-results',
    use: {
        baseURL: `http://127.0.0.1:${PORT}`,
        trace: 'retain-on-failure',
        screenshot: 'only-on-failure'
    },
    webServer: {
        command: `pnpm exec next start --hostname 127.0.0.1 --port ${PORT}`,
        url: `http://127.0.0.1:${PORT}/robots.txt`,
        reuseExistingServer: false,
        timeout: 60_000
    },
    projects: [
        {
            name: 'mobile',
            use: {
                ...devices['Desktop Chrome'],
                viewport: { width: 390, height: 844 },
                isMobile: true,
                hasTouch: true,
                deviceScaleFactor: 3
            }
        },
        {
            name: 'desktop',
            use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } }
        }
    ]
});
