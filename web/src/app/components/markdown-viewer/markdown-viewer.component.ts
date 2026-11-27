import { NgTemplateOutlet, isPlatformBrowser } from '@angular/common';
import {
  ChangeDetectionStrategy,
  Component,
  PLATFORM_ID,
  computed,
  inject,
  input,
  signal,
} from '@angular/core';

export type InlineSpanKind = 'text' | 'code' | 'bold' | 'italic' | 'strike' | 'link';

export interface InlineSpan {
  readonly kind: InlineSpanKind;
  readonly text: string;
  readonly href?: string;
}

export type AlertTone = 'note' | 'tip' | 'important' | 'warning' | 'caution';

export interface MarkdownListItem {
  readonly indent: number;
  readonly ordered: boolean;
  readonly orderNumber: number;
  readonly checked: boolean | null;
  readonly spans: readonly InlineSpan[];
}

export type TableAlign = 'left' | 'center' | 'right';

export type MarkdownBlock =
  | { readonly id: number; readonly type: 'heading'; readonly level: 1 | 2 | 3 | 4 | 5 | 6; readonly spans: readonly InlineSpan[] }
  | { readonly id: number; readonly type: 'paragraph'; readonly lines: readonly (readonly InlineSpan[])[] }
  | { readonly id: number; readonly type: 'code'; readonly lang: string; readonly code: string }
  | { readonly id: number; readonly type: 'hr' }
  | { readonly id: number; readonly type: 'blockquote'; readonly alert: AlertTone | null; readonly lines: readonly (readonly InlineSpan[])[] }
  | { readonly id: number; readonly type: 'list'; readonly items: readonly MarkdownListItem[] }
  | {
      readonly id: number;
      readonly type: 'table';
      readonly headers: readonly (readonly InlineSpan[])[];
      readonly aligns: readonly TableAlign[];
      readonly rows: readonly (readonly (readonly InlineSpan[])[])[];
    };

/**
 * Pure template-bound GitHub-Flavored Markdown renderer with zero `innerHTML` or
 * `bypassSecurityTrustHtml` usage, eliminating XSS vectors by construction.
 */
@Component({
  selector: 'app-markdown-viewer',
  standalone: true,
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    @if (blocks().length === 0) {
      <div class="py-8 text-center text-xs text-zinc-400">
        {{ emptyLabel() }}
      </div>
    } @else {
      <div class="space-y-3 text-[13px] leading-relaxed text-zinc-800 dark:text-zinc-200 break-words">
        @for (block of blocks(); track block.id) {
          @switch (block.type) {
            @case ('heading') {
              @switch (block.level) {
                @case (1) {
                  <h1 class="text-lg font-semibold tracking-tight text-zinc-950 dark:text-zinc-50 pb-1.5 border-b border-zinc-200 dark:border-zinc-800 mt-1">
                    @for (sp of block.spans; track $index) {
                      <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                    }
                  </h1>
                }
                @case (2) {
                  <h2 class="text-base font-semibold tracking-tight text-zinc-950 dark:text-zinc-100 pb-1 border-b border-zinc-200/80 dark:border-zinc-800/80 pt-1">
                    @for (sp of block.spans; track $index) {
                      <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                    }
                  </h2>
                }
                @case (3) {
                  <h3 class="text-sm font-semibold text-zinc-900 dark:text-zinc-100 pt-0.5">
                    @for (sp of block.spans; track $index) {
                      <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                    }
                  </h3>
                }
                @default {
                  <h4 class="text-xs font-semibold uppercase tracking-wide text-zinc-600 dark:text-zinc-300 pt-0.5">
                    @for (sp of block.spans; track $index) {
                      <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                    }
                  </h4>
                }
              }
            }

            @case ('paragraph') {
              <p class="leading-relaxed text-zinc-700 dark:text-zinc-300">
                @for (line of block.lines; track $index; let last = $last) {
                  @for (sp of line; track $index) {
                    <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                  }
                  @if (!last) {
                    <br />
                  }
                }
              </p>
            }

            @case ('hr') {
              <hr class="border-zinc-200 dark:border-zinc-800 my-2" />
            }

            @case ('code') {
              <div class="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-zinc-950 text-zinc-100 overflow-hidden">
                <div class="px-3 py-1.5 bg-zinc-900 border-b border-zinc-800 flex items-center justify-between text-[11px] font-mono text-zinc-400">
                  <span>{{ block.lang || 'text' }}</span>
                  <button
                    type="button"
                    (click)="copyCode(block.id, block.code)"
                    class="px-2 py-0.5 rounded hover:bg-zinc-800 text-zinc-300 transition-colors"
                  >
                    {{ copiedBlockId() === block.id ? 'Copied ✓' : 'Copy' }}
                  </button>
                </div>
                <pre class="p-3 text-xs font-mono leading-relaxed overflow-x-auto m-0"><code>{{ block.code }}</code></pre>
              </div>
            }

            @case ('blockquote') {
              @if (block.alert; as tone) {
                <div class="p-3 rounded-lg border space-y-1 text-xs" [class]="alertClasses(tone)">
                  <div class="font-mono text-[10px] font-semibold uppercase tracking-wider">
                    {{ tone }}
                  </div>
                  @for (line of block.lines; track $index) {
                    <div>
                      @for (sp of line; track $index) {
                        <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                      }
                    </div>
                  }
                </div>
              } @else {
                <blockquote class="pl-3.5 py-0.5 border-l-2 border-zinc-300 dark:border-zinc-700 text-zinc-600 dark:text-zinc-400 italic space-y-1">
                  @for (line of block.lines; track $index) {
                    <div>
                      @for (sp of line; track $index) {
                        <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                      }
                    </div>
                  }
                </blockquote>
              }
            }

            @case ('list') {
              <div class="space-y-1.5">
                @for (item of block.items; track $index) {
                  <div
                    class="flex items-start gap-2"
                    [class]="indentClass(item.indent)"
                  >
                    @if (item.checked !== null) {
                      <span
                        class="mt-0.5 inline-flex items-center justify-center w-3.5 h-3.5 rounded border text-[10px] font-mono shrink-0"
                        [class]="
                          item.checked
                            ? 'bg-emerald-500/15 border-emerald-500/50 text-emerald-600 dark:text-emerald-300'
                            : 'bg-zinc-50 dark:bg-zinc-900 border-zinc-300 dark:border-zinc-700 text-transparent'
                        "
                      >
                        ✓
                      </span>
                    } @else if (item.ordered) {
                      <span class="font-mono text-xs text-zinc-400 dark:text-zinc-500 shrink-0 min-w-[1.25rem] text-right">
                        {{ item.orderNumber }}.
                      </span>
                    } @else {
                      <span class="mt-2 w-1.5 h-1.5 rounded-full bg-zinc-400 dark:bg-zinc-500 shrink-0"></span>
                    }

                    <div
                      class="min-w-0 flex-1"
                      [class]="item.checked === true ? 'line-through text-zinc-400 dark:text-zinc-500' : 'text-zinc-700 dark:text-zinc-300'"
                    >
                      @for (sp of item.spans; track $index) {
                        <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                      }
                    </div>
                  </div>
                }
              </div>
            }

            @case ('table') {
              <div class="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800">
                <table class="w-full text-xs border-collapse">
                  <thead class="bg-zinc-100/80 dark:bg-zinc-900 text-zinc-700 dark:text-zinc-300">
                    <tr>
                      @for (hdr of block.headers; track $index; let colIdx = $index) {
                        <th
                          class="px-3 py-2 font-semibold border-b border-zinc-200 dark:border-zinc-800"
                          [class]="alignClass(block.aligns[colIdx])"
                        >
                          @for (sp of hdr; track $index) {
                            <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                          }
                        </th>
                      }
                    </tr>
                  </thead>
                  <tbody class="divide-y divide-zinc-200/70 dark:divide-zinc-800/80">
                    @for (row of block.rows; track $index) {
                      <tr class="bg-white/60 dark:bg-zinc-950/50">
                        @for (cell of row; track $index; let colIdx = $index) {
                          <td class="px-3 py-2 text-zinc-700 dark:text-zinc-300" [class]="alignClass(block.aligns[colIdx])">
                            @for (sp of cell; track $index) {
                              <ng-container [ngTemplateOutlet]="spanTpl" [ngTemplateOutletContext]="{ $implicit: sp }" />
                            }
                          </td>
                        }
                      </tr>
                    }
                  </tbody>
                </table>
              </div>
            }
          }
        }
      </div>
    }

    <ng-template #spanTpl let-sp>
      @switch (sp.kind) {
        @case ('code') {
          <code class="px-1.5 py-0.5 rounded font-mono text-[11.5px] bg-zinc-100 dark:bg-zinc-800/90 border border-zinc-200/80 dark:border-zinc-700/80 text-zinc-900 dark:text-zinc-100">{{ sp.text }}</code>
        }
        @case ('bold') {
          <strong class="font-semibold text-zinc-950 dark:text-zinc-100">{{ sp.text }}</strong>
        }
        @case ('italic') {
          <em class="italic">{{ sp.text }}</em>
        }
        @case ('strike') {
          <del class="line-through text-zinc-400">{{ sp.text }}</del>
        }
        @case ('link') {
          @if (sp.href) {
            <a
              [href]="sp.href"
              target="_blank"
              rel="noopener noreferrer"
              class="text-sky-600 dark:text-sky-400 underline underline-offset-2 hover:text-sky-500"
            >{{ sp.text }}</a>
          } @else {
            <span>{{ sp.text }}</span>
          }
        }
        @default {
          <span>{{ sp.text }}</span>
        }
      }
    </ng-template>
  `,
  imports: [NgTemplateOutlet],
})
export class MarkdownViewerComponent {
  private readonly platformId = inject(PLATFORM_ID);

  readonly markdown = input<string>('');
  readonly emptyLabel = input<string>('No Markdown content stored yet.');

  readonly copiedBlockId = signal<number | null>(null);
  readonly blocks = computed<readonly MarkdownBlock[]>(() => parseMarkdownDocument(this.markdown()));

  copyCode(blockId: number, code: string): void {
    if (!isPlatformBrowser(this.platformId)) {
      return;
    }
    void navigator.clipboard?.writeText(code).then(() => {
      this.copiedBlockId.set(blockId);
      window.setTimeout(() => {
        if (this.copiedBlockId() === blockId) {
          this.copiedBlockId.set(null);
        }
      }, 1500);
    });
  }

  indentClass(indent: number): string {
    switch (indent) {
      case 1:
        return 'pl-5';
      case 2:
        return 'pl-10';
      case 3:
        return 'pl-14';
      default:
        return '';
    }
  }

  alignClass(align?: TableAlign): string {
    switch (align) {
      case 'center':
        return 'text-center';
      case 'right':
        return 'text-right';
      default:
        return 'text-left';
    }
  }

  alertClasses(tone: AlertTone): string {
    switch (tone) {
      case 'tip':
        return 'bg-emerald-500/10 border-emerald-500/30 text-emerald-800 dark:text-emerald-200';
      case 'important':
        return 'bg-violet-500/10 border-violet-500/30 text-violet-800 dark:text-violet-200';
      case 'warning':
        return 'bg-amber-500/10 border-amber-500/30 text-amber-800 dark:text-amber-200';
      case 'caution':
        return 'bg-rose-500/10 border-rose-500/30 text-rose-800 dark:text-rose-200';
      default:
        return 'bg-sky-500/10 border-sky-500/30 text-sky-800 dark:text-sky-200';
    }
  }
}

const FENCE_OPEN_RE = /^\s*(`{3,}|~{3,})\s*([^\s`~]*)\s*$/;
const HR_RE = /^\s*(?:-\s*-\s*-[-\s]*|\*\s*\*\s*\*[\*\s]*|_\s*_\s*_[_\s]*)$/;
const HEADING_RE = /^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$/;
const BLOCKQUOTE_RE = /^\s{0,3}>\s?(.*)$/;
const ALERT_RE = /^\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*(.*)$/i;
const LIST_ITEM_RE = /^(\s*)(?:([-*+])|(\d+)[.)])\s+(.*)$/;
const TASK_BOX_RE = /^\[([ xX])\]\s+(.*)$/;
const TABLE_SEP_CELL_RE = /^:?-{3,}:?$/;

export function parseMarkdownDocument(raw: string): readonly MarkdownBlock[] {
  if (!raw || !raw.trim()) {
    return [];
  }

  const lines = raw.replace(/\r\n?/g, '\n').split('\n');
  const blocks: MarkdownBlock[] = [];
  let nextId = 1;
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    if (!line.trim()) {
      i++;
      continue;
    }

    // 1. Fenced code block (``` or ~~~)
    const fenceMatch = FENCE_OPEN_RE.exec(line);
    if (fenceMatch) {
      const marker = fenceMatch[1];
      const fenceChar = marker[0];
      const minLen = marker.length;
      const lang = fenceMatch[2] || '';
      const codeLines: string[] = [];
      i++;
      while (i < lines.length) {
        const candidate = lines[i].trim();
        if (
          candidate.length >= minLen &&
          candidate.startsWith(fenceChar.repeat(minLen)) &&
          candidate.replaceAll(fenceChar, '').trim() === ''
        ) {
          i++;
          break;
        }
        codeLines.push(lines[i]);
        i++;
      }
      blocks.push({
        id: nextId++,
        type: 'code',
        lang,
        code: codeLines.join('\n'),
      });
      continue;
    }

    // 2. Horizontal rule
    if (HR_RE.test(line)) {
      blocks.push({ id: nextId++, type: 'hr' });
      i++;
      continue;
    }

    // 3. ATX Heading (# .. ######)
    const headingMatch = HEADING_RE.exec(line);
    if (headingMatch) {
      const level = Math.min(Math.max(headingMatch[1].length, 1), 6) as 1 | 2 | 3 | 4 | 5 | 6;
      blocks.push({
        id: nextId++,
        type: 'heading',
        level,
        spans: parseInlineSpans(headingMatch[2]),
      });
      i++;
      continue;
    }

    // 4. GFM Table (Header row + | --- | separator row)
    if (i + 1 < lines.length && looksLikeTableRow(line) && isTableSeparatorLine(lines[i + 1])) {
      const rawHeaders = splitTableCells(line);
      const aligns = parseTableAlignments(lines[i + 1], rawHeaders.length);
      const headers = rawHeaders.map((cell) => parseInlineSpans(cell));
      const rows: (readonly (readonly InlineSpan[])[])[] = [];
      i += 2;
      while (i < lines.length && lines[i].trim() && looksLikeTableRow(lines[i])) {
        const cells = splitTableCells(lines[i]);
        const normalizedCells = headers.map((_, colIdx) => parseInlineSpans(cells[colIdx] ?? ''));
        rows.push(normalizedCells);
        i++;
      }
      blocks.push({
        id: nextId++,
        type: 'table',
        headers,
        aligns,
        rows,
      });
      continue;
    }

    // 5. Blockquote & GitHub Alerts (> [!NOTE] etc.)
    const bqMatch = BLOCKQUOTE_RE.exec(line);
    if (bqMatch) {
      const quoteRawLines: string[] = [];
      while (i < lines.length) {
        const m = BLOCKQUOTE_RE.exec(lines[i]);
        if (!m) break;
        quoteRawLines.push(m[1]);
        i++;
      }
      let alertTone: AlertTone | null = null;
      if (quoteRawLines.length > 0) {
        const alertMatch = ALERT_RE.exec(quoteRawLines[0].trim());
        if (alertMatch) {
          alertTone = alertMatch[1].toLowerCase() as AlertTone;
          const restFirst = alertMatch[2].trim();
          if (restFirst) {
            quoteRawLines[0] = restFirst;
          } else {
            quoteRawLines.shift();
          }
        }
      }
      const parsedQuoteLines = quoteRawLines
        .filter((l, idx) => l.trim().length > 0 || (idx > 0 && idx < quoteRawLines.length - 1))
        .map((l) => parseInlineSpans(l));
      blocks.push({
        id: nextId++,
        type: 'blockquote',
        alert: alertTone,
        lines: parsedQuoteLines,
      });
      continue;
    }

    // 6. Unordered / Ordered / Task Checklist items
    if (LIST_ITEM_RE.test(line)) {
      const items: MarkdownListItem[] = [];
      while (i < lines.length) {
        const lm = LIST_ITEM_RE.exec(lines[i]);
        if (!lm) {
          break;
        }
        const rawIndent = lm[1].replace(/\t/g, '  ').length;
        const indent = Math.min(Math.floor(rawIndent / 2), 3);
        const ordered = Boolean(lm[3]);
        const orderNumber = ordered ? Number(lm[3]) : 0;
        let body = lm[4];
        let checked: boolean | null = null;
        const taskMatch = TASK_BOX_RE.exec(body);
        if (taskMatch) {
          checked = taskMatch[1].toLowerCase() === 'x';
          body = taskMatch[2];
        }
        items.push({
          indent,
          ordered,
          orderNumber,
          checked,
          spans: parseInlineSpans(body),
        });
        i++;
      }
      blocks.push({
        id: nextId++,
        type: 'list',
        items,
      });
      continue;
    }

    // 7. Standard paragraph (collect until blank line or block starter)
    const paraLines: (readonly InlineSpan[])[] = [];
    while (i < lines.length) {
      const cur = lines[i];
      if (
        !cur.trim() ||
        FENCE_OPEN_RE.test(cur) ||
        HR_RE.test(cur) ||
        HEADING_RE.test(cur) ||
        BLOCKQUOTE_RE.test(cur) ||
        LIST_ITEM_RE.test(cur) ||
        (i + 1 < lines.length && looksLikeTableRow(cur) && isTableSeparatorLine(lines[i + 1]))
      ) {
        break;
      }
      paraLines.push(parseInlineSpans(cur.trim()));
      i++;
    }
    if (paraLines.length > 0) {
      blocks.push({
        id: nextId++,
        type: 'paragraph',
        lines: paraLines,
      });
    }
  }

  return blocks;
}

function looksLikeTableRow(line: string): boolean {
  const trimmed = line.trim();
  return trimmed.includes('|') && trimmed !== '|';
}

function splitTableCells(line: string): string[] {
  let trimmed = line.trim();
  if (trimmed.startsWith('|')) trimmed = trimmed.slice(1);
  if (trimmed.endsWith('|')) trimmed = trimmed.slice(0, -1);
  return trimmed.split('|').map((c) => c.trim());
}

function isTableSeparatorLine(line: string): boolean {
  if (!looksLikeTableRow(line)) return false;
  const cells = splitTableCells(line);
  return cells.length > 0 && cells.every((c) => TABLE_SEP_CELL_RE.test(c));
}

function parseTableAlignments(sepLine: string, count: number): TableAlign[] {
  const cells = splitTableCells(sepLine);
  const out: TableAlign[] = [];
  for (let idx = 0; idx < count; idx++) {
    const c = (cells[idx] ?? '').trim();
    if (c.startsWith(':') && c.endsWith(':')) {
      out.push('center');
    } else if (c.endsWith(':')) {
      out.push('right');
    } else {
      out.push('left');
    }
  }
  return out;
}

export function parseInlineSpans(text: string): readonly InlineSpan[] {
  if (!text) {
    return [];
  }
  const out: InlineSpan[] = [];
  let plainBuf = '';
  let idx = 0;

  const flushPlain = () => {
    if (plainBuf) {
      out.push({ kind: 'text', text: plainBuf });
      plainBuf = '';
    }
  };

  while (idx < text.length) {
    const ch = text[idx];

    // Inline code: `...`
    if (ch === '`') {
      const end = text.indexOf('`', idx + 1);
      if (end > idx + 1) {
        flushPlain();
        out.push({ kind: 'code', text: text.slice(idx + 1, end) });
        idx = end + 1;
        continue;
      }
    }

    // Link: [label](url)
    if (ch === '[') {
      const closeBracket = text.indexOf(']', idx + 1);
      if (closeBracket > idx + 1 && text[closeBracket + 1] === '(') {
        const closeParen = text.indexOf(')', closeBracket + 2);
        if (closeParen > closeBracket + 2) {
          const label = text.slice(idx + 1, closeBracket);
          const rawUrl = text.slice(closeBracket + 2, closeParen).trim();
          const safeHref = sanitizeSafeHref(rawUrl);
          flushPlain();
          if (safeHref) {
            out.push({ kind: 'link', text: label, href: safeHref });
          } else {
            out.push({ kind: 'text', text: label });
          }
          idx = closeParen + 1;
          continue;
        }
      }
    }

    // Bold: **...** or __...__
    if ((ch === '*' && text[idx + 1] === '*') || (ch === '_' && text[idx + 1] === '_')) {
      const delim = ch + ch;
      const end = text.indexOf(delim, idx + 2);
      if (end > idx + 2) {
        flushPlain();
        out.push({ kind: 'bold', text: text.slice(idx + 2, end) });
        idx = end + 2;
        continue;
      }
    }

    // Strikethrough: ~~...~~
    if (ch === '~' && text[idx + 1] === '~') {
      const end = text.indexOf('~~', idx + 2);
      if (end > idx + 2) {
        flushPlain();
        out.push({ kind: 'strike', text: text.slice(idx + 2, end) });
        idx = end + 2;
        continue;
      }
    }

    // Italic: *...* or _..._
    if ((ch === '*' || ch === '_') && text[idx + 1] && text[idx + 1] !== ' ' && text[idx + 1] !== ch) {
      const end = text.indexOf(ch, idx + 1);
      if (end > idx + 1 && text[end - 1] !== ' ') {
        flushPlain();
        out.push({ kind: 'italic', text: text.slice(idx + 1, end) });
        idx = end + 1;
        continue;
      }
    }

    plainBuf += ch;
    idx++;
  }

  flushPlain();
  return out;
}

function sanitizeSafeHref(raw: string): string | null {
  if (!raw) return null;
  const trimmed = raw.trim();
  const lower = trimmed.toLowerCase();
  if (lower.startsWith('https://') || lower.startsWith('http://') || lower.startsWith('mailto:')) {
    return trimmed;
  }
  return null;
}
