# Reader UI specification

## Mobile first

Target 360-430 px wide portrait screens first. Desktop should become a centered reading column, not a stretched mobile UI.

Reader layout:

```text
[ back ]        Chapter 7           [ ... ]

          surrounding text, quietly muted
      current sentence in stronger emphasis
             current WORD softly emphasized

-----------------------------
  -15   play/pause   +30      [Controls]
          progress / duration
```

Keep the reader immersive: title and chapter metadata stay compact in the
header; playback settings are collapsed until requested. A restrained ambient
background tint is welcome, but avoid heavy blur, animated gradients, or
effects that compete with the text. Keep surrounding sentences legible and
high-contrast; use weight and gentle color shifts rather than washing them out.

## Highlight style

Avoid a karaoke gimmick that destroys readability. The active word can use weight/background/underline, but the paragraph should still look like a book when paused. Consider three modes:

- Word (default)
- Sentence
- Minimal (only a subtle marker)

## DOM/windowing

Do not mount an entire 150k-word book as spans. Render active chapter, or an active block window, and retain timing arrays separately. The sync engine binary-searches timing data; DOM lookup maps word IDs only for the mounted window.

## Accessibility

- semantic paragraphs/headings;
- sufficient contrast;
- keyboard play/pause/seek on desktop;
- normal text selection should work;
- respect `prefers-reduced-motion` by disabling smooth auto-scroll transitions;
- do not hide all player controls behind hover.
