# Concept A — Sunlit Kitten

## Thesis

Waffle’s homepage should feel like morning light on cream paper and a soft introduction to a real kitten — not a product launch. The cat is the focal point; the agent is the quiet project named after her. Affection first, technical detail only as a murmur underneath.

## Voice sample

> This is Waffle.
>
> Named after a lively little kitten.
>
> A little agent with a big personality — chat, tools, a sandbox, the works. Built for me, named for her.
>
> What she gets into: company when you want it, careful pokes at tools, soft notes she keeps.
>
> Come say hello. She’s usually around. So is the project.

## Palette tokens

| Token | Value | Role |
| --- | --- | --- |
| `paper` | `#FBF7F0` | Page ground — warm cream |
| `paper-warm` | `#F5EDE0` | Soft close / footer strip |
| `ink` | `#1A1612` | Soft near-black display + body |
| `ink-muted` | `#5C4A3A` | Secondary lines, nav links |
| `label` | `#8D481F` / warm brown | Section labels |
| `ginger` | `#E99A42` | Cheerful accent only (underline, heart, label) |
| `ginger-light` | `#F5C579` | Optional sun wash |
| `window` | soft white sheer | Ambient morning light (texture, not a color block) |

Brand coat tokens (on the cat only): `ginger-base` `#E99A42`, `stripe` `#8D481F`, `muzzle-cream` `#F8E2B9`, `eye` `#7E8A68`, `nose` `#D98278`.

## Typography direction

- **Display:** Soft modern rounded grotesk — human, slightly plump terminals, not geometric Swiss and not Inter-as-hero. Think “friendly book cover” more than “startup landing.”
- **Body:** Same family or a calm humanist sans at readable size; generous line-height on cream.
- **Labels:** Small, title-case or lowercase, warm brown; never shouty caps.
- **Weight:** Heavy display for “This is Waffle.” / section titles; regular for sublines. Avoid thin corporate light weights.
- **Scale:** Hero display large and calm (not billboard). Secondary lines clearly subordinate.

## Hero architecture

```
┌────────────────────────────────────────────────────────────┐
│  waffle                    Docs · Notes · Source           │
│                                                            │
│   This is Waffle.                    [standing kitten]     │
│   ──── (ginger stroke)               large, full body      │
│   Named after a lively little kitten.                      │
│   a personal AI agent project                              │
│                                                            │
│   [sheer morning window light from left]                   │
└────────────────────────────────────────────────────────────┘
```

- **Left:** Friendly large type + two-tier subcopy (affectionate primary, tiny technical tertiary).
- **Right:** Canon standing pose as clear focal; soft ground shadow; airy negative space.
- **Nav:** Plain text only — Docs, Notes, Source.
- **Accent:** One thin ginger stroke under the headline; ginger never floods the ground.

## Section map

| # | File | Section | Intent |
| --- | --- | --- | --- |
| 01 | `01-hero.jpg` | Hero | Introduce the cat; name the project. |
| 02 | `02-what-it-is.jpg` | What it is | Personal agent, short and warm; sitting kitten. |
| 03 | `03-what-she-gets-into.jpg` | Capabilities / story | Three soft story rows, not a feature grid. |
| 04 | `04-why-waffle.jpg` | Name story | Why the project is named after her. |
| 05 | `05-soft-close.jpg` | Soft close + footer | Gentle hello; Source · Notes · Docs; personal-project footer. |

Suggested scroll order: Hero → What it is → What she gets into → Why Waffle → Soft close.

## How brand assets are used

| Section | Asset | Notes |
| --- | --- | --- |
| Hero | `poses/standing.png` | Full-body focal; illustration style preserved |
| What it is | `poses/sitting-airplane-ears.png` (target) / sitting | Sitting presence; soft shadow |
| Capabilities | `expressions/curious.png` | Large bust / head; grey-green eyes |
| Why Waffle | `poses/sitting.png` | Companion to name story |
| Soft close | `poses/standing.png` + pleased energy | Smaller goodbye presence |

All cat art must stay on-canon: forehead M, pale muzzle, grey-green eyes, pink nose, banded legs, ringed tail, long kitten proportions. Ginger `#E99A42` is coat + accent only — never a purple/blue AI treatment.

## Motion notes (optional)

- Hero: slow light shift (window wash), optional tiny tail tip sway; no bounce loops.
- Section labels: fade-up on scroll, 200–300ms ease-out.
- Soft close heart: single gentle pulse once on enter, then still.
- Prefer stillness with warmth over mascot hyperactivity.

## Risks

- **Photoreal drift:** Generators may smooth the illustrated cat toward photo-real; always re-anchor to canon PNGs.
- **SaaS gravity:** Soft cards can tip into feature-grid marketing — keep copy feline and short; tertiary tech only.
- **Ginger overload:** Too much `#E99A42` flattens the “accent only” rule; keep ground cream.
- **Airplane-ears fidelity:** Sitting airplane-ears pose is distinctive; don’t silently replace with generic upright ears in production assets.
- **Type:** Rounded display can look childish if too ballooned — stay soft modern, not toy.

## Image index

| File | Section | Source generation |
| --- | --- | --- |
| `01-hero.jpg` | Hero — “This is Waffle.” | image_edit from `poses/standing.png` |
| `02-what-it-is.jpg` | What it is | image_edit from `poses/sitting-airplane-ears.png` |
| `03-what-she-gets-into.jpg` | Capabilities / story | image_edit from `expressions/curious.png` + sitting |
| `04-why-waffle.jpg` | Name story | image_edit from `poses/sitting.png` |
| `05-soft-close.jpg` | Soft close + footer | image_edit from standing + pleased |

## Self-critique (after reading hero)

The hero reads as a personal introduction, not a product pitch: “This is Waffle.” plus “Named after a lively little kitten.” and a quiet tertiary line do the work without hard CTAs, hardware slogans, or conversion chrome. Cream ground, sheer morning light, and a large standing kitten make the page feel like someone showing you their cat first. Risks to watch: keep production art locked to the illustrated canon (not photoreal), keep ginger as a thin accent, and resist any later urge to add “Get started” energy — this still needs to stay beloved, not sold.
