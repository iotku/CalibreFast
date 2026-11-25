/**
 * How a notification reads: the notifications menu (nav.ts) and system
 * notifications (sw.ts) word it alike, as templates/notifications.html does.
 */

export interface Notice {
    id: number;
    /** placeholder_added, favorite_author, favorite_series or hardcover_relink (notifications.go). */
    kind: string;
    title: string;
    on_deck: boolean;
    seen: boolean;
    href: string;
    /** favorite_*: the author or series, and how many of its books are new. */
    subject?: string;
    count: number;
    /** placeholder_added: the saved book's authors ("A & B"), if known. */
    authors?: string;
}
/** A notice's headline and the line under it (may be empty). */
export function noticeText(n: Notice): [headline: string, note: string] {
    const many = n.count > 1;
    const subject = n.subject ?? "";
    switch (n.kind) {
        case "favorite_author":
            return many ? [`${n.count} new books by ${subject}`, "An author you follow"] : [n.title, `New, by ${subject}`];
        case "hardcover_relink":
            return ["Link Hardcover again", `To ${n.title}`];
        case "favorite_series":
            return many ? [`${n.count} new books in ${subject}`, "A series you follow"] : [n.title, `New in ${subject}`];
        default:
            // As notifications.html words it.
            return [`Saved book added to library: ${n.title}${n.authors ? ` by ${n.authors}` : ""}${n.on_deck ? " · On deck" : ""}`, ""];
    }
}
