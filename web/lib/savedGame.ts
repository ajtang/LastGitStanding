// Remembers the run in progress, so getting back to it doesn't depend on keeping the URL.
// Losing this is harmless, because the ?game= link still works, so storage that's blocked
// or full just means no Resume button.
const KEY = "lgs.currentGame";

export function loadSavedGame(): string | null {
  try {
    return window.localStorage.getItem(KEY);
  } catch {
    return null;
  }
}

export function saveGame(id: string) {
  try {
    window.localStorage.setItem(KEY, id);
  } catch {}
}

// Takes the id so an old tab reaching game over can't forget a newer run.
export function forgetGame(id: string) {
  try {
    if (window.localStorage.getItem(KEY) === id) window.localStorage.removeItem(KEY);
  } catch {}
}
