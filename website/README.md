# Waffle website

Personal project homepage for [Waffle](https://github.com/matt-riley/waffle) — a small AI agent named after a lively kitten.

Design direction: **Concept A · Sunlit Kitten** (see `../design-exploration/A-sunlit-kitten/`).

## Develop

```sh
cd website
npm install
npm run dev
```

Open the URL Astro prints (usually `http://localhost:4321`).

## Build

```sh
npm run build
npm run preview
```

Set the public site origin for production builds so canonical and social URLs
are absolute:

```sh
PUBLIC_SITE_URL=https://waffle.example.com npm run build
```

## Brand art

Approved kitten art is copied into `src/assets/waffle/` from `../assets/brand/waffle/` for the Astro build. Prefer re-copying from the brand tree if canon assets change; do not invent new cat designs.
