// Quick edits rewrite the manifest text and go through the same commit as anything
// else. They're typing assistance, not a side door. Comments survive because this
// is line editing, which is also why it's regex and not a YAML round trip.

export function bumpNumber(yaml: string, key: string, delta: number, floor = 0): string {
  const re = new RegExp(`^(\\s*(?:- )?${key}:\\s*)(\\d+)`, "m");
  return yaml.replace(re, (_, head: string, n: string) => `${head}${Math.max(floor, Number(n) + delta)}`);
}

export function setValue(yaml: string, key: string, value: string): string {
  const re = new RegExp(`^(\\s*(?:- )?${key}:\\s*)("[^"]*"|\\S+)`, "m");
  return yaml.replace(re, (_, head: string) => `${head}${value}`);
}

export function getValue(yaml: string, key: string): string | null {
  const m = yaml.match(new RegExp(`^\\s*(?:- )?${key}:\\s*("[^"]*"|\\S+)`, "m"));
  return m ? m[1].replace(/"/g, "") : null;
}

export function addRegion(yaml: string, name: string): string {
  if (new RegExp(`name:\\s*${name}\\b`).test(yaml)) return yaml;
  const block = `  - name: ${name}\n    nodes: 3\n    nodeSize: standard\n    towers: 1\n`;
  return yaml.replace(/^(regions:\n(?:(?:  .*|\s*#.*)\n)*)/m, `$1${block}`);
}

export interface DiffLine { kind: "same" | "add" | "del"; text: string }

// Plain LCS. These are forty-line files; anything fancier would be for my own amusement.
export function diffLines(before: string, after: string): DiffLine[] {
  const a = before.split("\n");
  const b = after.split("\n");
  const dp = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const out: DiffLine[] = [];
  let i = 0, j = 0;
  while (i < a.length || j < b.length) {
    if (i < a.length && j < b.length && a[i] === b[j]) {
      out.push({ kind: "same", text: a[i] });
      i++; j++;
    } else if (i < a.length && (j >= b.length || dp[i + 1][j] >= dp[i][j + 1])) {
      out.push({ kind: "del", text: a[i++] });
    } else {
      out.push({ kind: "add", text: b[j++] });
    }
  }
  return out;
}
