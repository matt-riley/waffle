# Desk mockups

Static HTML/CSS frames for the Today layout work in #532–#536. They keep Waffle's cream / ink / ginger identity and do not invent ChatGPT chrome.

Open the `.html` files in a browser, or use the PNGs in issues and reviews.

| Frame | File | Ships |
|---|---|---|
| **Hearth** | [01-hearth](01-hearth.html) · [png](01-hearth.png) | Default Today. History in the ink rail. Centered reading column. One-card composer. No permanent Session aside. Target for #533, #534, #535. |
| **Evening** | [02-evening](02-evening.html) · [png](02-evening.png) | Same bones as Hearth, docs-site evening tokens. Target for #532. |
| **Split kiln** | [03-split](03-split.html) · [png](03-split.png) | Hearth with the artifact canvas open. Target for #536. |
| **Ember** | [04-ember](04-ember.html) · [png](04-ember.png) | Optional later diet: icon rail + hideable history strip. Not the first slice. |

Shared rules the implementation must keep:

- Brand mark is `assets/brand/waffle/poses/sitting.png`. Do not copy it into this folder.
- Ginger is the only loud fill. No zinc / slate palette.
- Ctrl/Cmd+K stays the command palette.
- `#desk-send` still reads `Send message` / `Queue follow-up`.
- Attachments, dictate, export, branch, regenerate, and artifact preview stay; these frames relocate them.

Hearth is the target. Evening is its dark map. Split is Hearth with a file open. Ember waits until Hearth still feels busy.
