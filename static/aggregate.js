const path = window.location.pathname.replace("/", ""); // "authors", "publishers", "tags"
const title = path.charAt(0).toUpperCase() + path.slice(1);
document.getElementById("page-title").textContent = title;
document.title = title;

const grid = document.getElementById("aggregate-grid");

const res = await fetch(`/api/${path}`);
const items = await res.json();

if (!items.length) {
    grid.innerHTML = "<p style='text-align:center; color:#888; padding:2rem'>Nothing found.</p>";
    throw new Error("empty");
}

const maxCount = items[0].count; // already sorted desc

for (const item of items) {
    const ratio = item.count / maxCount;
    const fontSize = 12 + ratio * 24; // scale from 12px to 36px
    const opacity = 0.4 + ratio * 0.6; // scale from 0.4 to 1.0

    const el = document.createElement("a");
    el.href = `/${path.slice(0, -1)}/${encodeURIComponent(item.name)}`;
    el.textContent = `${item.name} (${item.count})`;
    el.style.cssText = `
        font-size: ${fontSize}px;
        opacity: ${opacity};
        color: #eee;
        text-decoration: none;
        padding: 4px 8px;
        display: inline-block;
        transition: opacity 0.15s;
    `;
    el.addEventListener("mouseenter", () => el.style.opacity = "1");
    el.addEventListener("mouseleave", () => el.style.opacity = opacity);

    grid.appendChild(el);
}