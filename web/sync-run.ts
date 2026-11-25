/**
 * Book downloads (book-sync.ts) for a page that doesn't start them itself
 * (the category lists, the readers, the account pages): nav.ts loads this
 * only when a run is asked for there (Sync now, downloads resumed, the
 * connection back), so those pages don't load the downloader otherwise.
 * Pages with books start their own (startBookSync) and answer
 * SYNC_NOW_EVENT, so this isn't loaded there.
 */

export { syncBooks } from "./book-sync.js";
