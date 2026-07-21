# Skill Metadata Context-Budget Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite the 19 repository-local skill descriptions so their aggregate metadata stays below 2,928 bytes without disabling implicit invocation.

**Architecture:** Treat each `SKILL.md` frontmatter description as an independent trigger contract. Enforce a shared static contract, replace only those 19 single lines, then validate YAML and aggregate size.

**Tech Stack:** Markdown, YAML, POSIX shell, Python skill validator

## Global Constraints

- Modify only `description` lines under `.codex/skills/*/SKILL.md`.
- Every description starts with `Use when`, stays on one YAML line, and is at most 200 characters.
- Aggregate description text must be less than 2,928 bytes.
- Keep names, bodies, licenses, file locations, and implicit invocation unchanged.
- Keep the currently untracked `.codex/skills` tree unstaged and uncommitted.

---

### Task 1: Establish the failing metadata baseline

**Files:**
- Inspect: `.codex/skills/*/SKILL.md`

**Interfaces:**
- Consumes: the 19 current single-line `description` fields
- Produces: a failing contract result and the original aggregate byte count

- [ ] **Step 1: Run the metadata contract against the current descriptions**

```bash
fail=0; total=0; count=0
for file in .codex/skills/*/SKILL.md; do
  description=$(sed -n 's/^description:[[:space:]]*//p' "$file" | head -1)
  bytes=$(printf '%s' "$description" | wc -c | tr -d ' ')
  count=$((count + 1))
  total=$((total + bytes))
  [[ $description == "Use when"* ]] || { echo "missing trigger prefix: $file"; fail=1; }
  (( ${#description} <= 200 )) || { echo "description too long: $file (${#description})"; fail=1; }
done
echo "skills=$count description_bytes=$total"
(( count == 19 )) || fail=1
(( total < 2928 )) || fail=1
exit "$fail"
```

Expected: FAIL because current descriptions do not all start with `Use when`, several exceed 200 characters, and the aggregate is 5,856 bytes.

- [ ] **Step 2: Capture the pre-edit skill tree for scope comparison**

```bash
comparison_dir=$(mktemp -d)
cp -R .codex/skills "$comparison_dir/skills"
printf '%s\n' "$comparison_dir"
```

Expected: prints the temporary directory containing the untouched skill files. Retain this path through Task 3.

### Task 2: Rewrite the 19 trigger descriptions

**Files:**
- Modify: `.codex/skills/awwwards-hero/SKILL.md`
- Modify: `.codex/skills/copywriting-personal/SKILL.md`
- Modify: `.codex/skills/emil-design-eng/SKILL.md`
- Modify: `.codex/skills/frontend-design/SKILL.md`
- Modify: `.codex/skills/gsap-core/SKILL.md`
- Modify: `.codex/skills/gsap-frameworks/SKILL.md`
- Modify: `.codex/skills/gsap-performance/SKILL.md`
- Modify: `.codex/skills/gsap-plugins/SKILL.md`
- Modify: `.codex/skills/gsap-scrolltrigger/SKILL.md`
- Modify: `.codex/skills/gsap-timeline/SKILL.md`
- Modify: `.codex/skills/gsap-utils/SKILL.md`
- Modify: `.codex/skills/gsap/SKILL.md`
- Modify: `.codex/skills/high-end-soft/SKILL.md`
- Modify: `.codex/skills/imagegen-frontend/SKILL.md`
- Modify: `.codex/skills/impeccable-designer/SKILL.md`
- Modify: `.codex/skills/interaction-design/SKILL.md`
- Modify: `.codex/skills/make-interfaces-feel-better/SKILL.md`
- Modify: `.codex/skills/redesign-existing/SKILL.md`
- Modify: `.codex/skills/responsive-design/SKILL.md`

**Interfaces:**
- Consumes: each skill's existing name and body semantics
- Produces: a concise trigger-only `description` for each skill

- [ ] **Step 1: Replace each description with the approved trigger contract**

| Skill directory | New description |
| --- | --- |
| `awwwards-hero` | Use when designing or implementing a website hero, landing header, or above-the-fold section from a brief or visual reference. |
| `copywriting-personal` | Use when writing or revising Waffle website copy that should feel personal, warm, and non-promotional. |
| `emil-design-eng` | Use when polishing interface components, interaction details, or motion using Emil Kowalski's design-engineering principles. |
| `frontend-design` | Use when creating a new interface or giving an existing UI a distinctive visual direction through layout, typography, color, and composition. |
| `gsap-core` | Use when implementing or reviewing GSAP core tweens, easing, stagger, transforms, or matchMedia behavior in web interfaces. |
| `gsap-frameworks` | Use when integrating GSAP with Vue, Nuxt, Svelte, SvelteKit, or another non-React component framework and its lifecycle. |
| `gsap-performance` | Use when diagnosing or optimizing GSAP animation jank, rendering cost, layout thrashing, FPS, or smoothness. |
| `gsap-plugins` | Use when implementing or reviewing GSAP plugins such as Flip, Draggable, SplitText, ScrollTo, SVG, physics, or custom easing plugins. |
| `gsap-scrolltrigger` | Use when implementing GSAP ScrollTrigger behavior including scroll-linked animation, scrub, pinning, parallax, or scroll triggers. |
| `gsap-timeline` | Use when sequencing or coordinating multi-step GSAP animations with timelines, position parameters, nesting, or playback controls. |
| `gsap-utils` | Use when using gsap.utils helpers such as clamp, mapRange, interpolate, random, snap, toArray, wrap, or pipe. |
| `gsap` | Use when adding or debugging an end-to-end GSAP animation system that spans multiple GSAP APIs or lacks a narrower specialist skill. |
| `high-end-soft` | Use when defining a high-end agency-style visual system for a website, including typography, spacing, depth, cards, and motion. |
| `imagegen-frontend` | Use when generating website mockups, section comps, landing-page concepts, or other frontend design reference images; not for code implementation. |
| `impeccable-designer` | Use when crafting a production frontend that must avoid generic AI styling, or when creating or applying an Impeccable Design Context. |
| `interaction-design` | Use when designing or implementing microinteractions, transitions, loading feedback, state changes, or other interaction behavior. |
| `make-interfaces-feel-better` | Use when polishing UI details such as spacing, borders, shadows, typography, hover states, motion, optical alignment, or perceived quality. |
| `redesign-existing` | Use when auditing and redesigning an existing website or app to improve visual quality without changing its core functionality. |
| `responsive-design` | Use when implementing adaptive layouts with mobile-first breakpoints, fluid typography, CSS Grid, or container queries. |

- [ ] **Step 2: Run the metadata contract again**

Run the Task 1 shell contract unchanged.

Expected: PASS with `skills=19` and `description_bytes` below `2928`.

### Task 3: Validate syntax and scope

**Files:**
- Validate: `.codex/skills/*/SKILL.md`

**Interfaces:**
- Consumes: rewritten YAML frontmatter and the untracked working tree
- Produces: per-skill validation evidence and a scope report

- [ ] **Step 1: Run the official quick validator for every skill**

```bash
for directory in .codex/skills/*; do
  [[ -f "$directory/SKILL.md" ]] || continue
  python /Users/mattriley/.codex/skills/.system/skill-creator/scripts/quick_validate.py "$directory" || exit 1
done
```

Expected: 19 `Skill is valid!` lines and exit code 0.

- [ ] **Step 2: Confirm only description lines changed**

```bash
scope_fail=0; changed=0
for file in .codex/skills/*/SKILL.md; do
  relative=${file#.codex/skills/}
  original="$comparison_dir/skills/$relative"
  old_description=$(sed -n 's/^description:[[:space:]]*//p' "$original" | head -1)
  new_description=$(sed -n 's/^description:[[:space:]]*//p' "$file" | head -1)
  [[ $old_description != "$new_description" ]] && changed=$((changed + 1))
  old_without_description=$(mktemp)
  new_without_description=$(mktemp)
  sed '/^description:/d' "$original" > "$old_without_description"
  sed '/^description:/d' "$file" > "$new_without_description"
  cmp -s "$old_without_description" "$new_without_description" || { echo "non-description change: $file"; scope_fail=1; }
  rm "$old_without_description" "$new_without_description"
done
(( changed == 19 )) || scope_fail=1
echo "changed_descriptions=$changed scope_fail=$scope_fail"
if (( scope_fail == 0 )); then
  rm -r "$comparison_dir"
fi
exit "$scope_fail"
```

Expected: `changed_descriptions=19 scope_fail=0` and exit code 0.

- [ ] **Step 3: Confirm the target tree remains untracked and unstaged**

```bash
git status --short -- .codex/skills docs/superpowers/plans/2026-07-22-skill-metadata-context-budget.md
git diff --cached --name-only -- .codex/skills
```

Expected: `.codex/` remains untracked and `git diff --cached` prints no skill files.
