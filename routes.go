package main

// Every route the server answers, in one place: its method and path, the
// guard that decides who may call it, and the handler (which lives with
// its feature). Add new routes here, guard included.
//
// Guards: requireUser (signed in), requireReader (book contents: signed
// in unless -public-read), requireEditor (editors and admins; their
// changes can be undone) and requireAdminUser in auth.go; requireWrites
// (admins, with -allow-writes) in main.go; requireLookups (lookup.go) and
// requireHardcover (hardcover.go) turn a route off with lookups or
// Hardcover. Admin-only guards answer anyone else like an unknown path.
// Hiding a button is not access control: the guard is.
//
// Anything that changes state is POST, PUT, PATCH or DELETE, never GET:
// CSRF protection (csrfProtect) relies on it.

import (
	"net/http"

	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/kosync"
)

// registerRoutes serves every route on http.DefaultServeMux.
func registerRoutes() {
	// Browsing and reading
	registerLibraryRoutes()
	registerRobotsRoutes()
	registerConvertedReadRoutes() // AZW3 and MOBI as EPUB, for the reader (convert_read.go)
	registerCategoryRoutes()      // authors, series, tags…: their pages, JSON and groups
	registerWorkerRoutes()        // offline reading (sw.go)

	// Editing the library
	registerBookEditRoutes() // uploads, covers and the metadata editor
	registerImportRoutes()   // the watch folder's import queue (imports.go)
	registerRecycleRoutes()  // removing books, and the Recycling Bin (recycle.go)
	registerEditLogRoutes()  // what was changed, by whom, and undoing it (editlog.go)
	registerMergeRoutes()
	registerAddFormatRoutes()
	registerPlaceholderRoutes()
	registerSeriesStatusRoutes()
	registerBulkEditRoutes()
	registerLibraryRepairRoutes()
	registerNotDuplicateRoutes()

	// Hardcover (users' own accounts: registerHardcoverSyncRoutes, with kosync's)
	registerHardcoverRoutes()
	registerAuthorProfileRoutes()

	// Running the server
	registerServerRoutes() // status, Stop and updates (server.go)
	registerBackupRoutes()

	// Signing in, accounts and everything users keep here
	registerAuthRoutes()
}

// ---------------------------------------------------------------------------
// Browsing and reading
// ---------------------------------------------------------------------------

// registerLibraryRoutes: the library's pages, covers, downloads, search
// and the readers.
func registerLibraryRoutes() {
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("GET /latest", latestHandler)
	http.HandleFunc("GET /api/latest", latestSortedHandler)
	http.HandleFunc("GET /shuffle", shuffleHandler)

	// The grid's pages of books, as JSON
	http.Handle("/pages/", servePages("pages"))
	http.HandleFunc("/library-info", libraryInfoHandler)

	// Scripts, styles and libraries (assets.go): cached for good.
	http.HandleFunc("GET /static/dist/", serveAsset)
	http.HandleFunc("GET /static/vendor/", serveAsset)
	http.HandleFunc("GET /static/icons/", serveAsset) // the app's icons (sw.go)

	// Books: covers, formats, downloads and their page
	http.HandleFunc("/cover/", coverHandler)            // Full cover image
	http.HandleFunc("/cover-thumb/", coverThumbHandler) // Thumbnail cover image
	http.HandleFunc("/formats/", formatsHandler)
	http.HandleFunc("/download/", requireReader(downloadHandler))
	http.HandleFunc("/book/get/", bookHandler)

	http.HandleFunc("GET /search", searchPageHandler)
	http.HandleFunc("GET /api/search", searchHandler)
	http.HandleFunc("GET /api/search/suggest", searchSuggestHandler) // the top bar's suggestions

	// The readers
	http.HandleFunc("/read", requireReader(readerPage("reader.html", true)))
	http.HandleFunc("/read-pdf", requireReader(readerPage("reader-pdf.html", false)))

	http.HandleFunc("/view/", requireReader(viewHandler))

	http.HandleFunc(cspReportPath, requireUser(cspReportHandler)) // csp.go
}

func registerRobotsRoutes() {
	http.HandleFunc("GET /robots.txt", robotsTxtHandler)
}

func registerConvertedReadRoutes() {
	http.HandleFunc("GET /book/as-epub/{uuid}/{format}", requireReader(asEPUBHandler))
}

// registerCategoryRoutes wires aggregate pages, filtered pages, JSON APIs and
// the group editor API for every category.
func registerCategoryRoutes() {
	for _, c := range categories {
		c := c
		http.HandleFunc("/"+c.Plural, aggregatePageHandler(c))
		http.HandleFunc("/"+c.URLPrefix+"/", filteredPageHandler(c))
		http.HandleFunc("/api/"+c.Plural, aggregateAPIHandler(c))
	}
	http.HandleFunc("GET /api/groups", groupsHandler)
	http.HandleFunc("GET /api/groups/{cat}/values", groupValuesHandler)
	http.HandleFunc("GET /api/languages/codes", languageCodesHandler)
	http.HandleFunc("PUT /api/groups/{cat}/{value...}", requireWrites(groupPutHandler))
	http.HandleFunc("PUT /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
	http.HandleFunc("DELETE /api/aggregates/{cat}/featured/{value...}", requireWrites(featuredHandler))
	http.HandleFunc("GET /"+aggregateImagesDir+"/{file}", aggregateImageHandler)
}

func registerWorkerRoutes() {
	http.HandleFunc("GET /sw.js", serveWorker)
	http.HandleFunc("GET /offline", offlineHandler)
	http.HandleFunc("GET /manifest.webmanifest", manifestHandler)
}

// ---------------------------------------------------------------------------
// Editing the library (editors and admins)
// ---------------------------------------------------------------------------

// registerBookEditRoutes: uploads, covers, the metadata editor and
// lookups, for editors and admins (requireEditor).
func registerBookEditRoutes() {
	// Uploads
	http.HandleFunc("/upload", requireEditor(uploadHandler))

	// Cover upload and management
	http.HandleFunc("/upload-cover/{uuid}", requireEditor(uploadCoverHandler))
	http.HandleFunc("/cover-status/{uuid}", coverStatusHandler)
	http.HandleFunc("/revert-cover/{uuid}", requireEditor(revertCoverHandler))

	// Individual Book metadata editor
	http.HandleFunc("/book/edit/{uuid}", requireEditor(bookEditPage))

	// Get metadata options for a book
	http.HandleFunc("/metadata/{uuid}", afterBookEdits(GetOptionsForBook))

	// Apply updated metadata to a book
	http.HandleFunc("/metadata/{uuid}/apply", requireEditor(afterBookEdits(applyMetadataHandler)))

	// ISBNs found in the book's own files (isbn.go)
	http.HandleFunc("/metadata/{uuid}/isbns", requireEditor(isbnCandidatesHandler))

	// Metadata from Open Library and Google Books, and Open Library covers
	// (lookup.go)
	http.HandleFunc("GET /metadata/lookup", requireEditor(requireLookups(lookupHandler)))
	http.HandleFunc("GET /metadata/lookup/cover", requireEditor(requireLookups(lookupCoverHandler)))
}

func registerImportRoutes() {
	http.HandleFunc("GET /import", requireEditor(importQueuePage))
	http.HandleFunc("GET /api/imports", requireEditor(importQueueHandler))
	http.HandleFunc("GET /import/edit", requireEditor(importEditPage))
	http.HandleFunc("GET /api/imports/table", requireEditor(importTableHandler))
	http.HandleFunc("POST /api/imports/bulk", requireEditor(importBulkHandler))
	http.HandleFunc("GET /import/{id}", requireEditor(importReviewPage))
	http.HandleFunc("GET /import/{id}/metadata", requireEditor(importMetadataHandler))
	http.HandleFunc("POST /import/{id}/apply", requireEditor(importApplyHandler))
	http.HandleFunc("POST /import/{id}/discard", requireEditor(importDiscardHandler))
	http.HandleFunc("POST /api/imports/discard-existing", requireEditor(importDiscardExistingHandler))
	http.HandleFunc("POST /import/{id}/accept", requireWrites(importAcceptHandler))
	http.HandleFunc("POST /api/imports/accept-done", requireWrites(importAcceptDoneHandler))
	http.HandleFunc("POST /import/{id}/discard-identical", requireEditor(importDiscardIdenticalHandler))
	http.HandleFunc("/import/{id}/isbns", requireEditor(importISBNsHandler))
	http.HandleFunc("GET /import/{id}/formats", requireEditor(importFormatsHandler))
	http.HandleFunc("GET /import/{id}/file/{format}", requireEditor(importFileHandler))
	http.HandleFunc("GET /import/{id}/as-epub/{format}", requireEditor(importAsEPUBHandler)) // convert_read.go
	http.HandleFunc("GET /import/{id}/cover", requireEditor(importCoverHandler))
	http.HandleFunc("POST /import/{id}/cover", requireEditor(importSaveCoverHandler))
	http.HandleFunc("GET /import/{id}/cover/status", requireEditor(importCoverStatusHandler))
	http.HandleFunc("POST /import/{id}/cover/revert", requireEditor(importRevertCoverHandler))
}

func registerRecycleRoutes() {
	http.HandleFunc("GET /recycle-bin", requireEditor(recycleBinPage))
	http.HandleFunc("GET /api/recycle-bin", requireEditor(recycleBinHandler))
	http.HandleFunc("POST /api/recycle-bin/{batch}/restore", requireEditor(afterBulkEdits(recycleRestoreHandler)))
	http.HandleFunc("POST /api/library/delete", requireEditor(afterBulkEdits(recycleDeleteHandler)))
	http.HandleFunc("POST /api/library/books/{uuid}/formats/{format}/remove", requireEditor(afterBulkEdits(recycleFormatHandler)))
}

func registerEditLogRoutes() {
	http.HandleFunc("GET /admin/activity", requireAdminUser(activityPage))
	http.HandleFunc("GET /api/admin/activity", requireAdminUser(activityHandler))
	http.HandleFunc("POST /api/admin/activity/revert", requireWrites(afterBulkEdits(activityRevertHandler)))
}

func registerMergeRoutes() {
	http.HandleFunc("GET /book/merge", requireEditor(mergePage))
	http.HandleFunc("GET /merge/metadata", requireEditor(mergeMetadataHandler))
	http.HandleFunc("POST /merge/apply", requireEditor(afterBulkEdits(mergeApplyHandler)))
}

func registerAddFormatRoutes() {
	http.HandleFunc("POST /book/add-format/{uuid}", requireEditor(addFormatUploadHandler))
	http.HandleFunc("GET /book/add-format/{uuid}/{id}", requireEditor(addFormatPage))
	http.HandleFunc("GET /book/add-format/{uuid}/{id}/metadata", requireEditor(addFormatMetadataHandler))
	http.HandleFunc("POST /book/add-format/{uuid}/{id}/apply", requireEditor(afterBulkEdits(addFormatApplyHandler)))
}

func registerPlaceholderRoutes() {
	http.HandleFunc("GET /missing", requireEditor(missingPageHandler))
	http.HandleFunc("GET /missing/new", requireEditor(newPlaceholderPageHandler))
	http.HandleFunc("GET /missing/{uuid}", placeholderPageHandler)
	http.HandleFunc("GET /"+placeholderCoversDir+"/{file}", placeholderCoverImageHandler)
	http.HandleFunc("POST /api/placeholders", requireEditor(placeholderCreateHandler))
	http.HandleFunc("PUT /api/placeholders/{uuid}", requireEditor(placeholderUpdateHandler))
	http.HandleFunc("DELETE /api/placeholders/{uuid}", requireEditor(placeholderDeleteHandler))
	http.HandleFunc("POST /api/placeholders/{uuid}/cover", requireEditor(placeholderCoverHandler))
	http.HandleFunc("DELETE /api/placeholders/{uuid}/cover", requireEditor(placeholderCoverHandler))
	http.HandleFunc("POST /api/placeholders/{uuid}/files", requireEditor(placeholderFilesHandler))
	http.HandleFunc("POST /api/placeholders/{uuid}/restore", requireEditor(placeholderRestoreHandler))
}

func registerSeriesStatusRoutes() {
	http.HandleFunc("POST /api/series/verified", requireEditor(seriesVerifyHandler))
	http.HandleFunc("DELETE /api/series/verified", requireEditor(seriesUnverifyHandler))
}

func registerBulkEditRoutes() {
	http.HandleFunc("GET /library/edit", requireEditor(bulkEditPage))
	http.HandleFunc("GET /api/library/table", requireEditor(libraryTableHandler))
	http.HandleFunc("POST /api/library/bulk", requireEditor(bulkEditHandler))
	http.HandleFunc("GET /api/library/bulk/jobs", requireEditor(bulkJobsHandler))
	http.HandleFunc("GET /api/library/bulk/failures", requireEditor(bulkFailuresHandler))
	http.HandleFunc("POST /api/library/bulk/failures/dismiss", requireEditor(bulkFailuresDismissHandler))
	http.HandleFunc("POST /api/library/repair-formats", requireWrites(afterBulkEdits(repairFormatsHandler)))
}

func registerLibraryRepairRoutes() {
	http.HandleFunc("GET /admin/repairs", requireWrites(libraryRepairsPage))
	http.HandleFunc("GET /api/admin/repairs", requireWrites(libraryRepairsCheck))
	http.HandleFunc("POST /api/admin/repairs/format-names", requireWrites(afterBulkEdits(repairFormatNamesHandler)))
}

func registerNotDuplicateRoutes() {
	http.HandleFunc("POST /api/library/not-duplicates", requireEditor(markNotDuplicatesHandler))
	http.HandleFunc("DELETE /api/library/not-duplicates/{id}", requireEditor(unmarkNotDuplicatesHandler))
}

// ---------------------------------------------------------------------------
// Hardcover
// ---------------------------------------------------------------------------

func registerHardcoverRoutes() {
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return requireEditor(requireLookups(requireHardcover(h))) }
	http.HandleFunc("GET /missing/hardcover", wrap(hardcoverReviewPage))
	http.HandleFunc("GET /missing/hardcover/author", wrap(hardcoverAuthorPage))
	http.HandleFunc("POST /api/hardcover/candidates", wrap(hardcoverJobHandler(hardcoverseries.JobCandidates)))
	http.HandleFunc("POST /api/hardcover/link", wrap(hardcoverJobHandler(hardcoverseries.JobLink)))
	http.HandleFunc("POST /api/hardcover/refresh", wrap(hardcoverJobHandler(hardcoverseries.JobRefresh)))
	http.HandleFunc("DELETE /api/hardcover/link", wrap(hardcoverUnlinkHandler))
	http.HandleFunc("POST /api/hardcover/batch", wrap(hardcoverBatchHandler))
	http.HandleFunc("GET /api/hardcover/jobs", wrap(hardcoverJobsHandler))
	http.HandleFunc("DELETE /api/hardcover/jobs/{id}", wrap(hardcoverCancelJobHandler))
	http.HandleFunc("POST /api/hardcover/apply", wrap(hardcoverApplyHandler))
	http.HandleFunc("POST /api/hardcover/undismiss", wrap(hardcoverUndismissHandler))
	http.HandleFunc("POST /api/hardcover/notmatch", wrap(hardcoverNotMatchHandler))
}

func registerAuthorProfileRoutes() {
	http.HandleFunc("POST /api/authors/profile", requireUser(requireLookups(requireHardcover(authorProfileHandler(false)))))
	http.HandleFunc("POST /api/authors/profile/refresh", requireEditor(requireLookups(requireHardcover(authorProfileHandler(true)))))
	http.HandleFunc("GET /"+hcAuthorImagesDir+"/{file}", authorImageHandler)
}

func registerHardcoverSyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/me/books/{uuid}/hardcover", requireUser(hardcoverShareHandler))
	mux.HandleFunc("GET /api/me/books/{uuid}/hardcover", requireUser(hardcoverBookStatusHandler))
	mux.HandleFunc("GET /api/me/books/{uuid}/hardcover/search", requireUser(hardcoverBookSearchHandler))
	mux.HandleFunc("PUT /api/me/books/{uuid}/hardcover/match", requireUser(hardcoverBookMatchHandler))
	mux.HandleFunc("DELETE /api/me/books/{uuid}/hardcover/match", requireUser(hardcoverBookMatchHandler))
	mux.HandleFunc("POST /account/hardcover", requireUser(hardcoverAccountHandler))
	mux.HandleFunc("GET /api/me/hardcover", requireUser(hardcoverStatusHandler))
}

// ---------------------------------------------------------------------------
// Accounts, and what users keep here
// ---------------------------------------------------------------------------

// registerAuthRoutes wires sign-in, account and admin endpoints.
func registerAuthRoutes() {
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("POST /register", registerHandler)
	http.HandleFunc("POST /logout", logoutHandler)
	http.HandleFunc("GET /api/me", meHandler)
	http.HandleFunc("/account", requireUser(accountHandler))
	http.HandleFunc("GET /admin/users", requireAdminUser(adminUsersPage))
	http.HandleFunc("POST /api/admin/users", requireAdminUser(adminCreateUser))
	http.HandleFunc("PATCH /api/admin/users/{id}", requireAdminUser(adminUpdateUser))
	http.HandleFunc("DELETE /api/admin/users/{id}", requireAdminUser(adminDeleteUser))
	http.HandleFunc("POST /api/admin/users/{id}/restore", requireAdminUser(adminRestoreUser))
	http.HandleFunc("POST /account/delete", requireUser(deleteOwnAccountHandler))
	registerOAuthRoutes()
	registerUserDataRoutes()
	registerProgressRoutes()
	registerGuestImportRoutes()
	registerAnnotationRoutes()
	registerShelfRoutes()
	registerDiscoverAdminRoutes()
	registerLinkRoutes()
	registerKosyncRoutes(http.DefaultServeMux)
	registerPrefsRoutes()
	registerStatsRoutes()
	registerNotificationRoutes()
}

func registerOAuthRoutes() {
	http.HandleFunc("GET /auth/{provider}/start", oauthStart)
	http.HandleFunc("GET /auth/{provider}/callback", oauthCallback)
	http.HandleFunc("POST /auth/{provider}/unlink", requireUser(oauthUnlink))
}

func registerUserDataRoutes() {
	http.HandleFunc("GET /me/{list}", requireUser(meListHandler))
	http.HandleFunc("GET /book/me/{uuid}", bookUserActionsHandler)
	http.HandleFunc("GET /api/me/books/{uuid}", requireUser(bookStateHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/favorite", requireUser(setFavoriteHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/status", requireUser(setStatusHandler))
	http.HandleFunc("PUT /api/me/books/{uuid}/not-interested", requireUser(setNotInterestedHandler))
	http.HandleFunc("POST /api/me/recommended/next", requireUser(recNextHandler))
}

func registerProgressRoutes() {
	http.HandleFunc("GET /api/me/reading", requireUser(readingHandler))
	http.HandleFunc("GET /api/me/home", requireUser(homeHandler))
	registerHomeLayoutRoutes()
	http.HandleFunc("GET /api/books", booksHandler)
	http.HandleFunc("GET /api/tag-strip", tagStripHandler)
	http.HandleFunc("GET /api/me/progress/{uuid}", requireUser(progressGetHandler))
	http.HandleFunc("PUT /api/me/progress/{uuid}", requireUser(progressPutHandler))
	http.HandleFunc("GET /api/me/bookmarks/{uuid}", requireUser(bookmarksGetHandler))
	http.HandleFunc("PUT /api/me/bookmarks/{uuid}/{id}", requireUser(bookmarkPutHandler))
	http.HandleFunc("DELETE /api/me/bookmarks/{uuid}/{id}", requireUser(bookmarkDeleteHandler))
}

func registerHomeLayoutRoutes() {
	http.HandleFunc("PUT /api/me/home-layout", requireUser(homeLayoutHandler))
	http.HandleFunc("PUT /api/me/hidden-tags/{name}", requireUser(hideTagHandler))
	http.HandleFunc("DELETE /api/me/hidden-tags/{name}", requireUser(unhideTagHandler))
}

func registerGuestImportRoutes() {
	http.HandleFunc("GET /account/import", requireUser(accountImportPage))
	http.HandleFunc("POST /api/me/import/preview", requireUser(guestImportPreviewHandler))
	http.HandleFunc("POST /api/me/import", requireUser(guestImportHandler))
}

func registerAnnotationRoutes() {
	http.HandleFunc("GET /api/me/annotations/{uuid}", requireUser(annotationsGetHandler))
	http.HandleFunc("PUT /api/me/annotations/{uuid}/{id}", requireUser(annotationPutHandler))
	http.HandleFunc("GET /api/me/annotations/{uuid}/koreader", requireUser(koreaderExportHandler))
	http.HandleFunc("POST /api/me/annotations/{uuid}/koreader", requireUser(koreaderImportHandler))
}

func registerShelfRoutes() {
	http.HandleFunc("GET /shelves", shelvesPageHandler)
	http.HandleFunc("GET /shelf/{id}", shelfPageHandler)
	http.HandleFunc("GET /api/shelves", shelvesListHandler)
	http.HandleFunc("POST /api/shelves", requireUser(shelfCreateHandler))
	http.HandleFunc("PATCH /api/shelves/{id}", requireUser(shelfUpdateHandler))
	http.HandleFunc("DELETE /api/shelves/{id}", requireUser(shelfDeleteHandler))
	http.HandleFunc("PUT /api/shelves/{id}/books/{uuid}", requireUser(shelfAddBookHandler))
	http.HandleFunc("DELETE /api/shelves/{id}/books/{uuid}", requireUser(shelfRemoveBookHandler))
	registerShelfSeriesRoutes()
	registerShelfPinRoutes()
	registerSeriesPinRoutes()
	registerAggregateFavoriteRoutes()
	registerListOrderRoutes()
}

func registerShelfSeriesRoutes() {
	http.HandleFunc("PUT /api/shelves/{id}/series/{name...}", requireUser(shelfAddSeriesHandler))
	http.HandleFunc("DELETE /api/shelves/{id}/series/{name...}", requireUser(shelfRemoveSeriesHandler))
	http.HandleFunc("GET /series-popup/{name...}", seriesPopupHandler)
}

func registerShelfPinRoutes() {
	http.HandleFunc("PUT /api/me/pins/{id}", requireUser(pinHandler))
	http.HandleFunc("DELETE /api/me/pins/{id}", requireUser(unpinHandler))
	http.HandleFunc("PUT /api/me/pins", requireUser(pinOrderHandler))
	http.HandleFunc("PUT /api/me/tag-pins/{name}", requireUser(tagPinHandler))
	http.HandleFunc("DELETE /api/me/tag-pins/{name}", requireUser(tagUnpinHandler))
	http.HandleFunc("PUT /api/me/shelves/order", requireUser(shelfOrderHandler))
	http.HandleFunc("PUT /api/admin/featured-shelves", requireAdminUser(featuredOrderHandler))
}

func registerSeriesPinRoutes() {
	http.HandleFunc("PUT /api/me/series-pins/{name}", requireUser(seriesPinHandler))
	http.HandleFunc("DELETE /api/me/series-pins/{name}", requireUser(seriesUnpinHandler))
	http.HandleFunc("PATCH /api/me/series-pins/{name}", requireUser(seriesPinPatchHandler))
	http.HandleFunc("DELETE /api/me/on-deck/{uuid}", requireUser(onDeckDismissHandler))
}

func registerAggregateFavoriteRoutes() {
	http.HandleFunc("GET /api/me/favorites", requireUser(aggFavoritesHandler))
	http.HandleFunc("PUT /api/me/favorites/{kind}/{name}", requireUser(aggFavoriteHandler))
	http.HandleFunc("DELETE /api/me/favorites/{kind}/{name}", requireUser(aggUnfavoriteHandler))
}

func registerListOrderRoutes() {
	http.HandleFunc("PUT /api/me/lists/{list}/order", requireUser(listOrderHandler))
	http.HandleFunc("PUT /api/shelves/{id}/order", requireUser(shelfBookOrderHandler))
}

func registerDiscoverAdminRoutes() {
	http.HandleFunc("GET /admin/discover", requireAdminUser(discoverAdminPage))
	http.HandleFunc("GET /api/admin/discover", requireAdminUser(discoverSettingsHandler))
	http.HandleFunc("PUT /api/admin/discover/settings", requireAdminUser(discoverSettingsPutHandler))
	http.HandleFunc("PUT /api/admin/discover/tags/{name}", requireAdminUser(discoverTagPutHandler))
	http.HandleFunc("DELETE /api/admin/discover/tags/{name}", requireAdminUser(discoverTagDeleteHandler))
}

func registerLinkRoutes() {
	http.HandleFunc("/l/{token}", linkHandler)
	http.HandleFunc("GET /api/admin/links", requireAdminUser(adminListLinks))
	http.HandleFunc("POST /api/admin/links", requireAdminUser(adminCreateLink))
	http.HandleFunc("DELETE /api/admin/links/{id}", requireAdminUser(adminRevokeLink))
}

func registerKosyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+kosync.Prefix+"/healthcheck", kosyncHealthHandler)
	mux.HandleFunc("POST "+kosync.Prefix+"/users/create", kosyncRegisterHandler)
	mux.HandleFunc("GET "+kosync.Prefix+"/users/auth", kosyncAuth(kosyncAuthorizeHandler))
	mux.HandleFunc("PUT "+kosync.Prefix+"/syncs/progress", kosyncAuth(kosyncPutHandler))
	mux.HandleFunc("GET "+kosync.Prefix+"/syncs/progress/{document}", kosyncAuth(kosyncGetHandler))
	mux.HandleFunc("POST /users/create", kosyncWrongURLHandler)
	mux.HandleFunc("GET /users/auth", kosyncWrongURLHandler)
	mux.HandleFunc("PUT /syncs/progress", kosyncWrongURLHandler)
	mux.HandleFunc("GET /syncs/progress/{document}", kosyncWrongURLHandler)
	mux.HandleFunc("POST /account/sync-key", requireUser(syncKeyHandler))
	registerHardcoverSyncRoutes(mux)
}

func registerPrefsRoutes() {
	http.HandleFunc("GET /preferences", preferencesPage)
	http.HandleFunc("PUT /api/me/prefs", requireUser(putPrefsHandler))
	http.HandleFunc("GET /api/me/saved-books", requireUser(savedBooksHandler))
}

func registerStatsRoutes() {
	http.HandleFunc("GET /me/stats", requireUser(statsPage))
	http.HandleFunc("GET /api/me/stats", requireUser(statsHandler))
	http.HandleFunc("PUT /api/me/activity", requireUser(activityPutHandler))
	http.HandleFunc("DELETE /api/me/activity", requireUser(activityDeleteHandler))
}

func registerNotificationRoutes() {
	http.HandleFunc("GET /me/notifications", requireUser(notificationsPage))
	http.HandleFunc("GET /api/me/notifications", requireUser(notificationsHandler))
	http.HandleFunc("POST /api/me/notifications/seen", requireUser(notificationsSeenHandler))
	http.HandleFunc("DELETE /api/me/notifications/{id}", requireUser(notificationDeleteHandler))
	http.HandleFunc("DELETE /api/me/notifications", requireUser(notificationsClearHandler))
	http.HandleFunc("GET /api/me/push", requireUser(pushInfoHandler))
	http.HandleFunc("PUT /api/me/push", requireUser(pushSubscribeHandler))
	http.HandleFunc("DELETE /api/me/push", requireUser(pushUnsubscribeHandler))
}

// ---------------------------------------------------------------------------
// Running the server (admins)
// ---------------------------------------------------------------------------

func registerServerRoutes() {
	http.HandleFunc("GET /admin/server", requireAdminUser(serverPage))
	http.HandleFunc("GET /admin/server/live", requireAdminUser(serverLive))
	http.HandleFunc("POST /api/admin/server/stop", requireAdminUser(serverStopHandler))
	http.HandleFunc("POST /api/admin/server/update", requireAdminUser(serverUpdateStart)) // server_update.go
	http.HandleFunc("GET /api/admin/server/update", requireAdminUser(serverUpdateStatus))
}

func registerBackupRoutes() {
	http.HandleFunc("POST /api/admin/backups", requireAdminUser(backupNowHandler))
	http.HandleFunc("GET /api/admin/backups/{name}", requireAdminUser(backupDownloadHandler))
}
