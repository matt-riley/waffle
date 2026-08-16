import { defineCollection } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

// Starlight owns one collection. Everything under src/content/docs/docs/
// therefore routes to /docs/..., which keeps the hand-built marketing page at /
// while Starlight owns the documentation tree. See website/DOCS-PLAN.md §5.
export const collections = {
	docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
};
