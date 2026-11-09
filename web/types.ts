// Shapes of the JSON the Go server sends and receives. Keep in sync with the
// Go structs named in each comment.

/** A grid entry (Go: Book in main.go). */
export interface Book {
    uuid: string;
    title: string;
    author_sort: string;
    series_index: number;
    pubdate: string;
    path: string;
    last_modified: string;
}

/** Signed-in user (Go: User in auth.go). */
export interface User {
    id: number;
    username: string;
    is_admin: boolean;
    disabled: boolean;
    has_password: boolean;
}

/** GET /api/me (Go: meHandler). */
export interface MeResponse {
    user: User | null;
    can_edit_library?: boolean;
    registration?: boolean;
}

/** A library value suggestion (Go: groupValuesHandler). */
export interface ValueSuggestion {
    value: string;
    label?: string;
}
