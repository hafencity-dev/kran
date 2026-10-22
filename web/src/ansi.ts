// Minimal terminal output model for job logs: SGR colors and bold/dim,
// carriage-return overwrites (progress bars), everything else stripped.

export interface Span {
  text: string
  cls: string
}

export type Line = Span[]

interface Pen {
  fg: number // -1 none, 0..15
  bold: boolean
  dim: boolean
}

const ESC = /\x1b(?:\[([0-9;?]*)([A-Za-z])|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()][A-Za-z0-9]|[=>78])/g

export class Terminal {
  lines: Line[] = []
  private cur: Line = []
  private pen: Pen = { fg: -1, bold: false, dim: false }
  private pending = ''
  constructor(private max = 5000) {}

  get dropped() {
    return this.droppedLines
  }
  private droppedLines = 0

  /** All complete lines plus the line being written. */
  view(): Line[] {
    return this.cur.length ? [...this.lines, this.cur] : this.lines
  }

  write(chunk: string) {
    let s = this.pending + chunk
    this.pending = ''
    // Keep an escape sequence split across chunks for the next write.
    const tail = s.lastIndexOf('\x1b')
    if (tail >= 0 && tail > s.length - 32 && !/[A-Za-z\x07\\]/.test(s.slice(tail + 2))) {
      this.pending = s.slice(tail)
      s = s.slice(0, tail)
    }
    let last = 0
    ESC.lastIndex = 0
    for (let m = ESC.exec(s); m; m = ESC.exec(s)) {
      this.text(s.slice(last, m.index))
      last = ESC.lastIndex
      if (m[2] === 'm') this.sgr(m[1] ?? '')
      else if (m[2] === 'K') {
        /* erase line: progress tools clear before redraw; \r handles it */
      }
    }
    this.text(s.slice(last))
  }

  private text(t: string) {
    if (!t) return
    for (let i = 0; i < t.length; ) {
      const nl = t.indexOf('\n', i)
      const cr = t.indexOf('\r', i)
      let end = t.length
      if (nl >= 0) end = nl
      if (cr >= 0 && cr < end) end = cr
      if (end > i) this.put(t.slice(i, end))
      if (end === t.length) break
      if (t[end] === '\n') this.newline()
      else if (t[end + 1] === '\n') {
        this.newline()
        i = end + 2
        continue
      } else this.cur = [] // bare \r: the next text overwrites the line
      i = end + 1
    }
  }

  private put(text: string) {
    const clean = text.replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '')
    if (!clean) return
    const cls = this.cls()
    const prev = this.cur[this.cur.length - 1]
    if (prev && prev.cls === cls) prev.text += clean
    else this.cur.push({ text: clean, cls })
  }

  private newline() {
    this.lines.push(this.cur)
    this.cur = []
    if (this.lines.length > this.max) {
      const n = this.lines.length - this.max
      this.lines.splice(0, n)
      this.droppedLines += n
    }
  }

  private cls(): string {
    const p = this.pen
    let c = ''
    if (p.fg >= 0) c += `a${p.fg} `
    if (p.bold) c += 'ab '
    if (p.dim) c += 'ad'
    return c.trim()
  }

  private sgr(params: string) {
    const ps = params === '' ? [0] : params.split(';').map(Number)
    for (let i = 0; i < ps.length; i++) {
      const p = ps[i]
      if (p === 0) this.pen = { fg: -1, bold: false, dim: false }
      else if (p === 1) this.pen.bold = true
      else if (p === 2) this.pen.dim = true
      else if (p === 22) this.pen.bold = this.pen.dim = false
      else if (p >= 30 && p <= 37) this.pen.fg = p - 30
      else if (p >= 90 && p <= 97) this.pen.fg = p - 90 + 8
      else if (p === 39) this.pen.fg = -1
      else if (p === 38 || p === 48) {
        // 256-color / truecolor: map the 16 base colors, ignore the rest.
        if (ps[i + 1] === 5) {
          if (p === 38 && ps[i + 2] < 16) this.pen.fg = ps[i + 2]
          i += 2
        } else if (ps[i + 1] === 2) i += 4
      }
    }
  }
}
