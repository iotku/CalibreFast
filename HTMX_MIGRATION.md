# Migration to htmx 4.0

## Overview

CalibreFast has been migrated from bespoke JavaScript infinite scroll to [htmx 4.0](https://four.htmx.org/), a modern hypermedia framework that enables dynamic page updates without custom JavaScript.

## What Changed

### Before (Bespoke JavaScript)
- ~300 lines of custom infinite scroll logic in `app.js`
- Manual IntersectionObserver management
- Complex page caching and placeholder system
- Custom abort controller logic for request cancellation
- Manual URL state management

### After (htmx 4.0)
- ~50 lines of JavaScript for modal and image loading only
- htmx handles all page loading, navigation, and URL state
- Server-side HTML rendering (no JSON → HTML conversion on client)
- Simpler, more maintainable codebase

## Architecture Changes

### Frontend

**Files Modified:**
- `templates/index.html` - Added htmx script tag and `hx-*` attributes for navigation
- `static/app-htmx.js` - New simplified version focusing only on modal and image handling
- `static/shared.js` - Removed `renderBooks()` and page management logic

**Files Added:**
- `templates/books-fragment.html` - Server-rendered HTML fragment for book grids

**Files Kept for Reference:**
- `static/app.js` - Original version (can be removed if no longer needed)

### Backend

**Go Changes (`main.go`):**

1. **New Endpoint:** `/htmx-page`
   - Reads pre-generated JSON pages
   - Renders HTML fragments using `books-fragment.html`
   - Returns HTML instead of JSON
   - Sets `HX-Push-Url` header for browser history

2. **Template Functions:**
   - `colorFromBook` - Generates consistent colors from book UUID
   - `add`, `sub`, `gt`, `lt`, `ge`, `le` - Template helper functions

3. **Updated Structs:**
   - `PageData` - Added `CurrentPage` and `TotalPages` fields
   - `BooksFragmentData` - New struct for fragment rendering

## Performance Considerations

### Maintained Features ✅
- **Pre-generated pages:** Still using static JSON files generated at startup
- **Lazy image loading:** IntersectionObserver still controls cover loading
- **Image queue:** MAX_CONCURRENT=8 image fetch queue preserved
- **Cover caching:** Server-side cover thumbnail caching unchanged
- **Infinite scroll:** htmx `revealed` trigger for seamless loading

### Improvements 🚀
- **Smaller JS bundle:** Reduced from ~300 to ~50 lines of app code
- **Server-side rendering:** Eliminates client-side JSON → HTML conversion
- **Built-in features:** URL state, request cancellation, and swap logic handled by htmx
- **Progressive enhancement:** Works better with browser features like back/forward

### Trade-offs ⚖️
- **Initial page load:** Small increase due to htmx library (~14KB minified)
- **Server CPU:** Minimal increase for template rendering (negligible with 30 books/page)

## htmx 4.0 Features Used

### Core Attributes
- `hx-get` - Make GET requests to load content
- `hx-trigger` - Control when requests fire (`load`, `change`, `revealed`)
- `hx-target` - Specify where to insert response content
- `hx-swap` - Control how content is swapped (`innerHTML`, `outerHTML`)
- `hx-push-url` - Update browser URL and history

### Key Patterns

**Initial Load:**
```html
<div hx-get="/htmx-page?page=1" 
     hx-trigger="load" 
     hx-swap="outerHTML">
    Loading...
</div>
```

**Infinite Scroll:**
```html
<div hx-get="/htmx-page?page={{ .NextPage }}" 
     hx-trigger="revealed" 
     hx-swap="outerHTML">
</div>
```

**Navigation:**
```html
<button hx-get="/htmx-page?page=2" 
        hx-target="#books" 
        hx-swap="innerHTML"
        hx-push-url="true">
```

## CDN Configuration

Using unpkg.com for htmx 4.0:
```html
<script src="https://unpkg.com/htmx.org@4.0.0/dist/htmx.min.js"></script>
```

**Why unpkg?**
- Version pinned to 4.0.0 (no surprise updates)
- Fast, reliable CDN with global edge locations
- No build step required

**Alternative:** Download htmx.min.js and serve locally for offline use.

## Event Handling

htmx emits custom events for lifecycle hooks:

```javascript
document.body.addEventListener('htmx:afterSwap', function(event) {
    // Re-initialize observers and handlers for new content
    const newImages = event.detail.target.querySelectorAll('.book-cover');
    newImages.forEach(img => coverObserver.observe(img));
});
```

## Testing

To verify the migration:

1. Start the server:
   ```bash
   go run . -basedir /path/to/calibre/library
   ```

2. Test infinite scroll:
   - Open http://localhost:8080
   - Scroll down - pages should load automatically
   - Verify URL updates as you scroll

3. Test navigation:
   - Use next/prev buttons
   - Type a page number and press Enter
   - Use browser back/forward buttons

4. Test modals:
   - Click on a book cover
   - Modal should open with book details
   - ESC key should close modal

## Future Improvements

- Consider htmx extensions (e.g., `loading-states` for skeleton UI)
- Add optimistic UI updates for faster perceived performance
- Explore htmx's `hx-boost` for search results
- Add `hx-indicator` for loading states

## References

- [htmx 4.0 Documentation](https://four.htmx.org/docs)
- [htmx Infinite Scroll Pattern](https://four.htmx.org/patterns/infinite-scroll)
- [htmx 4.0 Release Announcement](https://four.htmx.org/announcements/2026-08-28-htmx-4.0.0-is-released)
