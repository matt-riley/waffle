import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const websiteRoot = new URL('../', import.meta.url);

async function read(relativePath) {
	try {
		return await readFile(new URL(relativePath, websiteRoot), 'utf8');
	} catch (error) {
		if (error?.code === 'ENOENT') return '';
		throw error;
	}
}

function navDestinations(html, label) {
	const nav = html.match(new RegExp(`<nav aria-label="${label}"[\\s\\S]*?<\\/nav>`));
	assert.ok(nav, `${label} navigation is present`);

	return [...nav[0].matchAll(/href="([^"]+)"/g)].map((match) => match[1]).sort();
}

test('motion enhancement fails open when the GSAP module does not become ready', async () => {
	const [layout, motion] = await Promise.all([
		read('src/layouts/Layout.astro'),
		read('src/scripts/motion.ts'),
	]);

	assert.match(layout, /setTimeout/);
	assert.match(layout, /js-motion-ready/);
	assert.match(layout, /classList\.remove\('js-motion'\)/);
	assert.match(layout, /removeProperty\('opacity'\)/);
	assert.match(motion, /classList\.add\('js-motion-ready'\)/);
});

test('motion enhancement stays disabled after the fail-open guard runs', async () => {
	const motion = await read('src/scripts/motion.ts');

	assert.match(
		motion,
		/if \(prefersReducedMotion\(\)\) \{[\s\S]*?return;[\s\S]*?\}\s*if \(!root\.classList\.contains\('js-motion'\)\) return;\s*gsap\.registerPlugin/,
	);
	assert.doesNotMatch(motion, /root\.classList\.add\('js-motion'\)/);
});

test('navigation labels and section ids describe their destinations', async () => {
	const [header, footer, activities, namesake] = await Promise.all([
		read('src/components/SiteHeader.astro'),
		read('src/components/SiteFooter.astro'),
		read('src/components/WhatSheGetsInto.astro'),
		read('src/components/WhyWaffle.astro'),
	]);

	assert.doesNotMatch(header, /label:\s*'Notes'/);
	assert.match(header, /href:\s*'#why-waffle'.*label:\s*'Why Waffle'/);
	assert.match(footer, /href="#why-waffle"[^>]*>Why Waffle</);
	assert.match(activities, /id="what-she-does"/);
	assert.match(namesake, /id="why-waffle"/);
});

test('header and footer navigation expose the same destinations', async () => {
	const builtHome = await read('dist/index.html');

	assert.deepEqual(navDestinations(builtHome, 'Footer'), navDestinations(builtHome, 'Primary'));
});

test('approved personal copy appears on the page and in the copy document', async () => {
	const [builtHome, copy] = await Promise.all([read('dist/index.html'), read('COPY.md')]);

	for (const content of [builtHome, copy]) {
		assert.match(content, /One binary\. One very orange accomplice\./);
		assert.match(content, /I wrote the code\. She supplied the personality\./);
		assert.doesNotMatch(content, /A project that lives here\. A name that meows back\./);
	}
});

test('shared layout exposes a skip link and every page exposes the main target', async () => {
	const [layout, home, notFound] = await Promise.all([
		read('src/layouts/Layout.astro'),
		read('src/pages/index.astro'),
		read('src/pages/404.astro'),
	]);

	assert.match(layout, /href="#main-content"[^>]*>\s*Skip to content/);
	assert.match(home, /<main id="main-content" tabindex="-1"/);
	assert.match(notFound, /<main id="main-content" tabindex="-1"/);
});

test('layout emits canonical and social sharing metadata from a configurable site origin', async () => {
	const [layout, config, builtHome] = await Promise.all([
		read('src/layouts/Layout.astro'),
		read('astro.config.mjs'),
		read('dist/index.html'),
	]);

	assert.match(config, /PUBLIC_SITE_URL/);
	assert.match(layout, /rel="canonical"/);
	assert.match(layout, /property="og:title"/);
	assert.match(layout, /property="og:description"/);
	assert.match(layout, /property="og:image"/);
	assert.match(layout, /property="og:url"/);
	assert.match(layout, /name="twitter:card"/);
	assert.match(builtHome, /<meta property="og:title"/);
});

test('brand and navigation links provide 44px hit areas', async () => {
	const [header, styles] = await Promise.all([
		read('src/components/SiteHeader.astro'),
		read('src/styles/global.css'),
	]);

	assert.match(header, /class="brand-link/);
	assert.match(styles, /\.brand-link\s*\{[^}]*min-block-size:\s*2\.75rem;[^}]*min-inline-size:\s*2\.75rem;/s);
	assert.match(styles, /\.nav-link\s*\{[^}]*min-height:\s*2\.75rem;[^}]*min-width:\s*2\.75rem;/s);
});

test('the static build contains a branded 404 route with a path home', async () => {
	const [notFoundSource, notFoundBuild] = await Promise.all([
		read('src/pages/404.astro'),
		read('dist/404.html'),
	]);

	assert.match(notFoundSource, /Nothing to see here/);
	assert.match(notFoundSource, /href="\/"/);
	assert.match(notFoundBuild, /Nothing to see here/);
	assert.match(notFoundBuild, /Back home/);
});

test('new-tab links declare both opener and referrer protections', async () => {
	const files = await Promise.all([
		read('src/components/SiteHeader.astro'),
		read('src/components/SiteFooter.astro'),
		read('src/components/SoftClose.astro'),
	]);

	for (const source of files) {
		assert.match(source, /target="_blank"[^>]*rel="noopener noreferrer"/s);
	}
});

test('the website package cannot be published accidentally', async () => {
	const packageJSON = JSON.parse(await read('package.json'));

	assert.equal(packageJSON.private, true);
});

test('a blank configured site origin is treated as unset', () => {
	const moduleURL = new URL('src/site-url.mjs', websiteRoot).href;
	const output = execFileSync(
		process.execPath,
		[
			'--input-type=module',
			'--eval',
			`const { resolveSiteURL } = await import(${JSON.stringify(moduleURL)}); process.stdout.write(JSON.stringify([resolveSiteURL('   ') ?? null, resolveSiteURL(undefined) ?? null, resolveSiteURL(' https://example.com ') ?? null]));`,
		],
		{ encoding: 'utf8' },
	);

	assert.deepEqual(JSON.parse(output), [null, null, 'https://example.com']);
});

test('the astro config resolves its site origin through the tested helper', async () => {
	const config = await read('astro.config.mjs');

	// Match the intent, not the formatting: quote style, spacing, and semicolons
	// are a formatter's business, and the behaviour under test is only that the
	// config resolves its origin through the helper the test above covers.
	assert.match(config, /import\s*\{\s*resolveSiteURL\s*\}\s*from\s*['"][^'"]*site-url\.mjs['"]/);
	assert.match(config, /resolveSiteURL\(\s*process\.env\.PUBLIC_SITE_URL\s*,?\s*\)/);
});

/* ---------------------------------------------------------------------------
   Documentation site (/docs/). Rules from website/DOCS-PLAN.md.
   --------------------------------------------------------------------------- */

const TIER_ONE_PAGES = [
	'src/content/docs/docs/meet/what-waffle-is.md',
	'src/content/docs/docs/meet/keeping-her-safe.mdx',
];

const TIER_TWO_PAGES = ['src/content/docs/docs/under-the-hood/architecture.md'];

test('the docs mount leaves the hand-built homepage and 404 in place', async () => {
	const [docsLanding, home, notFound, config] = await Promise.all([
		read('dist/docs/index.html'),
		read('dist/index.html'),
		read('dist/404.html'),
		read('astro.config.mjs'),
	]);

	assert.match(docsLanding, /<title>[^<]*Waffle[^<]*<\/title>/);
	// The homepage must stay the bespoke one, not a Starlight-rendered route.
	assert.match(home, /This is Waffle\./);
	assert.match(notFound, /Nothing to see here/);
	assert.match(config, /disable404Route:\s*true/);
});

test('both navigations reach the docs', async () => {
	const builtHome = await read('dist/index.html');

	for (const label of ['Primary', 'Footer']) {
		assert.ok(
			navDestinations(builtHome, label).includes('/docs/'),
			`${label} navigation links to /docs/`,
		);
	}
});

test('brand tokens stay in sync between the marketing and docs stylesheets', async () => {
	const [global, docs] = await Promise.all([
		read('src/styles/global.css'),
		read('src/styles/docs.css'),
	]);

	const shared = {
		paper: 'color-paper',
		'paper-warm': 'color-paper-warm',
		ink: 'color-ink',
		'ink-muted': 'color-ink-muted',
		label: 'color-label',
		ginger: 'color-ginger',
		'ginger-light': 'color-ginger-light',
	};

	for (const [docsName, globalName] of Object.entries(shared)) {
		const globalValue = global.match(new RegExp(`--${globalName}:\\s*(#[0-9a-fA-F]{6});`))?.[1];
		const docsValue = docs.match(new RegExp(`--waffle-${docsName}:\\s*(#[0-9a-fA-F]{6});`))?.[1];

		assert.ok(globalValue, `global.css defines --${globalName}`);
		assert.ok(docsValue, `docs.css defines --waffle-${docsName}`);
		assert.equal(
			docsValue.toLowerCase(),
			globalValue.toLowerCase(),
			`--waffle-${docsName} must match --${globalName}`,
		);
	}
});

test('the docs stylesheet does not pull Tailwind into Starlight', async () => {
	const docs = await read('src/styles/docs.css');

	// Two resets in one page is how a themed docs site breaks. The marketing
	// page owns Tailwind; the docs own Starlight.
	assert.doesNotMatch(docs, /@import\s+["']tailwindcss["']/);
});

test('the docs theme defines both grounds so neither can be a media-query afterthought', async () => {
	const docs = await read('src/styles/docs.css');

	assert.match(docs, /:root\[data-theme="light"\]/);
	assert.match(docs, /--sl-color-black:\s*var\(--waffle-evening\)/);
	assert.match(docs, /--sl-color-black:\s*var\(--waffle-paper\)/);
});

test('ginger is never used for docs body text on the paper ground', async () => {
	const docs = await read('src/styles/docs.css');

	const lightBlock = docs.match(/:root\[data-theme="light"\][\s\S]*?\n\}/)?.[0];
	assert.ok(lightBlock, 'docs.css defines a light theme block');

	// #E99A42 measures 2.2:1 on paper: rules and fills only, never text.
	for (const textToken of ['--sl-color-text', '--sl-color-text-accent', '--sl-color-white']) {
		const value = lightBlock.match(new RegExp(`${textToken}:\\s*([^;]+);`))?.[1]?.trim();
		assert.ok(value, `light theme defines ${textToken}`);
		assert.doesNotMatch(
			value,
			/--waffle-ginger\b|--waffle-ginger-light\b|#e99a42|#f5c579/i,
			`${textToken} must not resolve to ginger on the paper ground`,
		);
	}
});

test('cat art stays within its per-page budget and never doubles up', async () => {
	for (const page of [...TIER_ONE_PAGES, ...TIER_TWO_PAGES]) {
		const source = await read(page);
		const callouts = [...source.matchAll(/<Callout\b/g)];

		assert.ok(
			callouts.length <= 2,
			`${page} uses ${callouts.length} cat callouts; the budget is 2`,
		);
		assert.doesNotMatch(
			source,
			/<\/Callout>\s*<Callout\b/,
			`${page} places two cat callouts back to back`,
		);
	}
});

test('every plain-language page descends into a technical counterpart', async () => {
	for (const page of TIER_ONE_PAGES) {
		const source = await read(page);

		assert.match(source, /Nerd corner/, `${page} offers a Nerd corner descent`);
		assert.match(source, /\/docs\/under-the-hood\//, `${page} links into the technical tier`);
	}
});

test('every technical page offers a way back up to plain language', async () => {
	for (const page of TIER_TWO_PAGES) {
		const source = await read(page);

		assert.match(source, /\/docs\/meet\//, `${page} links back to the plain-language tier`);
	}
});

test('every sidebar entry points at a page that exists', async () => {
	const config = await read('astro.config.mjs');
	const slugs = [...config.matchAll(/slug:\s*'([^']+)'/g)].map((match) => match[1]);

	assert.ok(slugs.length > 0, 'the sidebar declares at least one page');

	for (const slug of slugs) {
		const candidates = await Promise.all([
			read(`src/content/docs/${slug}.md`),
			read(`src/content/docs/${slug}.mdx`),
		]);

		assert.ok(
			candidates.some((content) => content.length > 0),
			`sidebar slug ${slug} resolves to a content file`,
		);
	}
});
