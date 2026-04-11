// Command palette: a Cmd/Ctrl-K overlay (with "/" as a secondary
// trigger when the user is not inside a text field) that lists every
// top-level action the app supports — navigation, auth, language
// switch, doc shortcuts, context-aware actions on the current page.
//
// Everything renders client-side from a static action catalog.
// Catalog is rebuilt on every open() so context-aware entries
// (current tenant, current app) always reflect the live URL.
//
// Translations live in PALETTE_STRINGS below, indexed by the BCP 47
// tag the server set on <html lang>. A miss falls back to English so
// every action stays usable even if a locale has gaps.
//
// Navigation goes through htmx.ajax() so a palette-driven jump
// produces the same partial swap as clicking a sidebar link. A full
// reload would feel like a regression against the rest of the UI.

import htmx from "htmx.org";

type PaletteAction = {
  id: string;
  labelKey: string;
  // Optional dynamic suffix appended to the localised label (e.g.
  // the tenant namespace). Kept separate from labelKey so one
  // translation key covers every tenant / app the user sees.
  labelSuffix?: string;
  hint?: string;
  handler: () => void;
};

// --- i18n --------------------------------------------------------------

// PALETTE_STRINGS holds the full translation table for every palette
// entry. Keep keys in sync across locales — an eslint / unit test
// check would be nice, but for now the table is short enough that
// drift is obvious at review time.
const PALETTE_STRINGS: Record<string, Record<string, string>> = {
  en: {
    "nav.dashboard": "Go to Dashboard",
    "nav.tenants": "Go to Tenants",
    "nav.marketplace": "Go to Marketplace",
    "nav.profile": "Go to Profile",
    "app.toggle-theme": "Toggle light / dark theme",
    "app.create-tenant": "Create a new tenant",
    "app.logout": "Log out",
    "app.copy-url": "Copy current URL",
    "app.refresh": "Refresh main content",
    "lang.en": "Language: English",
    "lang.ru": "Язык: Русский",
    "lang.kk": "Тіл: Қазақша",
    "lang.zh": "语言: 中文",
    "doc.auth": "Open docs — auth architecture",
    "doc.migrate": "Open docs — migrate to passthrough",
    "doc.troubleshoot": "Open docs — troubleshooting",
    "tenant.view": "Open tenant",
    "tenant.create-app": "Create application in tenant",
    "app.edit": "Edit this application",
    "app.delete": "Delete this application",
    "app.open-logs": "Open Logs tab",
    "app.open-connection": "Open Connection tab",
    "app.open-overview": "Open Overview tab",
    "hint.headerButton": "Header button",
    "hint.opensModal": "Opens a modal",
    "hint.newTab": "Opens in a new tab",
    "hint.clipboard": "Copied to clipboard",
    "hint.htmxSwap": "Re-fetches main content without a full reload",
  },
  ru: {
    "nav.dashboard": "Перейти в Обзор",
    "nav.tenants": "Перейти в Тенанты",
    "nav.marketplace": "Перейти в Маркетплейс",
    "nav.profile": "Перейти в Профиль",
    "app.toggle-theme": "Переключить светлую / тёмную тему",
    "app.create-tenant": "Создать новый тенант",
    "app.logout": "Выйти",
    "app.copy-url": "Скопировать текущий URL",
    "app.refresh": "Обновить содержимое",
    "lang.en": "Language: English",
    "lang.ru": "Язык: Русский",
    "lang.kk": "Тіл: Қазақша",
    "lang.zh": "语言: 中文",
    "doc.auth": "Открыть документацию — архитектура auth",
    "doc.migrate": "Открыть документацию — миграция на passthrough",
    "doc.troubleshoot": "Открыть документацию — troubleshooting",
    "tenant.view": "Открыть тенант",
    "tenant.create-app": "Создать приложение в тенанте",
    "app.edit": "Редактировать это приложение",
    "app.delete": "Удалить это приложение",
    "app.open-logs": "Открыть вкладку Логи",
    "app.open-connection": "Открыть вкладку Подключение",
    "app.open-overview": "Открыть вкладку Обзор",
    "hint.headerButton": "Кнопка в шапке",
    "hint.opensModal": "Открывает модальное окно",
    "hint.newTab": "Откроется в новой вкладке",
    "hint.clipboard": "Скопировано в буфер обмена",
    "hint.htmxSwap": "Перезагружает только main-content, без полной перезагрузки",
  },
  kk: {
    "nav.dashboard": "Басты бетке өту",
    "nav.tenants": "Тенанттарға өту",
    "nav.marketplace": "Маркетплейске өту",
    "nav.profile": "Профильге өту",
    "app.toggle-theme": "Ашық / қараңғы теманы ауыстыру",
    "app.create-tenant": "Жаңа тенант жасау",
    "app.logout": "Шығу",
    "app.copy-url": "Ағымдағы URL-ді көшіру",
    "app.refresh": "Негізгі мазмұнды жаңарту",
    "lang.en": "Language: English",
    "lang.ru": "Язык: Русский",
    "lang.kk": "Тіл: Қазақша",
    "lang.zh": "语言: 中文",
    "doc.auth": "Құжаттаманы ашу — auth архитектурасы",
    "doc.migrate": "Құжаттаманы ашу — passthrough көшу",
    "doc.troubleshoot": "Құжаттаманы ашу — ақаулықтарды жою",
    "tenant.view": "Тенантты ашу",
    "tenant.create-app": "Тенантта қолданба жасау",
    "app.edit": "Осы қолданбаны өңдеу",
    "app.delete": "Осы қолданбаны жою",
    "app.open-logs": "Логтар қойындысын ашу",
    "app.open-connection": "Қосылу қойындысын ашу",
    "app.open-overview": "Шолу қойындысын ашу",
    "hint.headerButton": "Жоғарғы жолақтағы түйме",
    "hint.opensModal": "Модальді ашады",
    "hint.newTab": "Жаңа қойындыда ашылады",
    "hint.clipboard": "Алмасу буферіне көшірілді",
    "hint.htmxSwap": "Толық қайта жүктеусіз main-content-ты жаңартады",
  },
  zh: {
    "nav.dashboard": "前往控制台",
    "nav.tenants": "前往租户",
    "nav.marketplace": "前往市场",
    "nav.profile": "前往个人资料",
    "app.toggle-theme": "切换浅色 / 深色主题",
    "app.create-tenant": "创建新租户",
    "app.logout": "退出登录",
    "app.copy-url": "复制当前链接",
    "app.refresh": "刷新主内容",
    "lang.en": "Language: English",
    "lang.ru": "Язык: Русский",
    "lang.kk": "Тіл: Қазақша",
    "lang.zh": "语言: 中文",
    "doc.auth": "打开文档 — 认证架构",
    "doc.migrate": "打开文档 — 迁移到 passthrough",
    "doc.troubleshoot": "打开文档 — 故障排查",
    "tenant.view": "打开租户",
    "tenant.create-app": "在租户中创建应用",
    "app.edit": "编辑此应用",
    "app.delete": "删除此应用",
    "app.open-logs": "打开日志标签页",
    "app.open-connection": "打开连接标签页",
    "app.open-overview": "打开概览标签页",
    "hint.headerButton": "顶栏按钮",
    "hint.opensModal": "打开一个弹窗",
    "hint.newTab": "在新标签页打开",
    "hint.clipboard": "已复制到剪贴板",
    "hint.htmxSwap": "重新拉取主内容，不做完整刷新",
  },
};

function t(key: string): string {
  const locale = document.documentElement.lang || "en";
  const table = PALETTE_STRINGS[locale] ?? PALETTE_STRINGS.en;
  return table[key] ?? PALETTE_STRINGS.en[key] ?? key;
}

// --- Doc URLs ----------------------------------------------------------

// Mirrors internal/view/page/profile.templ's docBaseURL. Kept in sync
// by eye; if either moves, update the other. Single source of truth is
// not worth a build-time pipeline for two short constants.
const DOC_BASE = "https://github.com/lexfrei/cozytempl/blob/master";

// --- Catalog -----------------------------------------------------------

function buildCatalog(): PaletteAction[] {
  const actions: PaletteAction[] = [
    {
      id: "nav.dashboard",
      labelKey: "nav.dashboard",
      hint: "/",
      handler: () => navigate("/"),
    },
    {
      id: "nav.tenants",
      labelKey: "nav.tenants",
      hint: "/tenants",
      handler: () => navigate("/tenants"),
    },
    {
      id: "nav.marketplace",
      labelKey: "nav.marketplace",
      hint: "/marketplace",
      handler: () => navigate("/marketplace"),
    },
    {
      id: "nav.profile",
      labelKey: "nav.profile",
      hint: "/profile",
      handler: () => navigate("/profile"),
    },
    {
      id: "app.toggle-theme",
      labelKey: "app.toggle-theme",
      hint: t("hint.headerButton"),
      handler: (): void => {
        document.getElementById("theme-toggle")?.click();
      },
    },
    {
      id: "app.create-tenant",
      labelKey: "app.create-tenant",
      hint: t("hint.opensModal"),
      handler: () => openModalById("create-tenant-modal"),
    },
    {
      id: "app.refresh",
      labelKey: "app.refresh",
      hint: t("hint.htmxSwap"),
      handler: () => refreshMainContent(),
    },
    {
      id: "app.copy-url",
      labelKey: "app.copy-url",
      hint: t("hint.clipboard"),
      handler: () => copyCurrentURL(),
    },
    {
      id: "app.logout",
      labelKey: "app.logout",
      hint: "POST /auth/logout",
      handler: () => logout(),
    },
  ];

  // Doc shortcuts — always visible so a user stuck anywhere in the app
  // can pop a doc open without having to remember the URL.
  actions.push(
    {
      id: "doc.auth",
      labelKey: "doc.auth",
      hint: t("hint.newTab"),
      handler: () => openInNewTab(DOC_BASE + "/docs/auth-architecture.md"),
    },
    {
      id: "doc.migrate",
      labelKey: "doc.migrate",
      hint: t("hint.newTab"),
      handler: () => openInNewTab(DOC_BASE + "/docs/migrating-to-passthrough-auth.md"),
    },
    {
      id: "doc.troubleshoot",
      labelKey: "doc.troubleshoot",
      hint: t("hint.newTab"),
      handler: () => openInNewTab(DOC_BASE + "/docs/troubleshooting.md"),
    },
  );

  // Language switcher entries. Skip the currently-active locale so
  // the user never sees "switch to the language I'm already in".
  const currentLang = document.documentElement.lang || "en";
  for (const lang of ["en", "ru", "kk", "zh"]) {
    if (lang === currentLang) continue;
    actions.push({
      id: `lang.${lang}`,
      labelKey: `lang.${lang}`,
      hint: `POST /lang?lang=${lang}`,
      handler: () => switchLanguage(lang),
    });
  }

  // Context-aware tenant actions.
  const tenant = currentTenantNamespace();
  if (tenant) {
    actions.push({
      id: "tenant.view",
      labelKey: "tenant.view",
      labelSuffix: ` — ${tenant}`,
      hint: `/tenants/${tenant}`,
      handler: () => navigate(`/tenants/${tenant}`),
    });
    actions.push({
      id: "tenant.create-app",
      labelKey: "tenant.create-app",
      labelSuffix: ` — ${tenant}`,
      hint: t("hint.opensModal"),
      handler: () => openModalById("create-app-modal"),
    });
  }

  // Context-aware app actions (when on /tenants/<ns>/apps/<name>).
  const app = currentAppContext();
  if (app) {
    actions.push(
      {
        id: "app.open-overview",
        labelKey: "app.open-overview",
        labelSuffix: ` — ${app.name}`,
        handler: () => navigate(`/tenants/${app.tenant}/apps/${app.name}?tab=overview`),
      },
      {
        id: "app.open-connection",
        labelKey: "app.open-connection",
        labelSuffix: ` — ${app.name}`,
        handler: () => navigate(`/tenants/${app.tenant}/apps/${app.name}?tab=connection`),
      },
      {
        id: "app.open-logs",
        labelKey: "app.open-logs",
        labelSuffix: ` — ${app.name}`,
        handler: () => navigate(`/tenants/${app.tenant}/apps/${app.name}?tab=logs`),
      },
      {
        id: "app.edit",
        labelKey: "app.edit",
        labelSuffix: ` — ${app.name}`,
        hint: t("hint.opensModal"),
        handler: () => {
          const trigger = document.querySelector<HTMLElement>("[data-action='app-edit']");
          trigger?.click();
        },
      },
    );
  }

  return actions;
}

// --- Action helpers ----------------------------------------------------

// currentTenantNamespace parses /tenants/<ns> or /tenants/<ns>/apps/…
// out of location.pathname.
function currentTenantNamespace(): string {
  const match = /^\/tenants\/([^/]+)/.exec(window.location.pathname);
  return match ? match[1] : "";
}

// currentAppContext parses /tenants/<ns>/apps/<name> out of the URL so
// the palette can add edit / logs / connection shortcuts for exactly
// the app the user is looking at.
function currentAppContext(): { tenant: string; name: string } | null {
  const match = /^\/tenants\/([^/]+)\/apps\/([^/?]+)/.exec(window.location.pathname);
  if (!match) return null;
  return { tenant: match[1], name: match[2] };
}

function navigate(path: string): void {
  void htmx.ajax("GET", path, {
    target: "#main-content",
    swap: "innerHTML",
    ...({ pushUrl: true } as Record<string, unknown>),
  });
}

function refreshMainContent(): void {
  navigate(window.location.pathname + window.location.search);
}

function copyCurrentURL(): void {
  void navigator.clipboard.writeText(window.location.href).catch(() => {
    // clipboard permission denied — silently ignore. The hint
    // already promised a clipboard copy so a failure stays
    // invisible rather than a toast spam.
  });
}

// logout submits a POST to /auth/logout via a short-lived form so
// the browser sends a proper state-changing request instead of a
// bare GET (which the router rejects).
function logout(): void {
  const form = document.createElement("form");
  form.method = "POST";
  form.action = "/auth/logout";
  document.body.appendChild(form);
  form.submit();
}

function switchLanguage(lang: string): void {
  const form = document.createElement("form");
  form.method = "POST";
  form.action = "/lang";
  const input = document.createElement("input");
  input.type = "hidden";
  input.name = "lang";
  input.value = lang;
  form.appendChild(input);
  document.body.appendChild(form);
  form.submit();
}

function openInNewTab(url: string): void {
  window.open(url, "_blank", "noopener,noreferrer");
}

function openModalById(id: string): void {
  const trigger = document.querySelector<HTMLElement>(`[data-action="modal-open"][data-modal="${id}"]`);
  if (trigger) {
    trigger.click();
    return;
  }

  const modal = document.getElementById(id);
  if (modal) modal.classList.add("is-open");
}

// --- Overlay -----------------------------------------------------------

let overlay: HTMLDivElement | null = null;
let searchInput: HTMLInputElement | null = null;
let listEl: HTMLUListElement | null = null;
let items: PaletteAction[] = [];
let filtered: PaletteAction[] = [];
let activeIndex = 0;

function ensureOverlay(): HTMLDivElement {
  if (overlay) return overlay;

  const root = document.createElement("div");
  root.className = "command-palette";
  root.setAttribute("role", "dialog");
  root.setAttribute("aria-modal", "true");
  root.setAttribute("aria-label", "Command palette");
  root.hidden = true;

  root.innerHTML = `
    <div class="command-palette-backdrop" data-palette-close="true"></div>
    <div class="command-palette-panel">
      <input type="text"
             class="command-palette-input"
             placeholder="Type a command..."
             autocomplete="off"
             spellcheck="false"
             aria-label="Filter commands" />
      <ul class="command-palette-list" role="listbox"></ul>
      <div class="command-palette-footer">
        <span><kbd>↑↓</kbd> navigate</span>
        <span><kbd>⏎</kbd> run</span>
        <span><kbd>esc</kbd> close</span>
      </div>
    </div>
  `;

  document.body.appendChild(root);

  overlay = root;
  searchInput = root.querySelector<HTMLInputElement>(".command-palette-input");
  listEl = root.querySelector<HTMLUListElement>(".command-palette-list");

  root.addEventListener("click", (evt) => {
    const target = evt.target as HTMLElement | null;
    if (target?.dataset.paletteClose === "true") close();
  });

  searchInput?.addEventListener("input", () => {
    updateFiltered();
    render();
  });

  searchInput?.addEventListener("keydown", (evt) => {
    switch (evt.key) {
      case "ArrowDown":
        evt.preventDefault();
        moveActive(1);
        break;
      case "ArrowUp":
        evt.preventDefault();
        moveActive(-1);
        break;
      case "Enter":
        evt.preventDefault();
        runActive();
        break;
      case "Escape":
        evt.preventDefault();
        close();
        break;
      default:
        break;
    }
  });

  return root;
}

function open(): void {
  const root = ensureOverlay();
  items = buildCatalog();
  activeIndex = 0;
  if (searchInput) searchInput.value = "";
  updateFiltered();
  render();
  root.hidden = false;
  document.body.classList.add("command-palette-open");
  requestAnimationFrame(() => searchInput?.focus());
}

function close(): void {
  if (!overlay) return;
  overlay.hidden = true;
  document.body.classList.remove("command-palette-open");
}

// labelFor renders the full visible label for a catalog entry: the
// translated labelKey plus any context-specific suffix.
function labelFor(item: PaletteAction): string {
  const base = t(item.labelKey);
  return item.labelSuffix ? base + item.labelSuffix : base;
}

function updateFiltered(): void {
  const query = searchInput?.value.trim().toLowerCase() ?? "";
  if (!query) {
    filtered = items.slice();
  } else {
    filtered = items.filter((item) => {
      const label = labelFor(item).toLowerCase();
      const hint = item.hint?.toLowerCase() ?? "";
      return label.includes(query) || hint.includes(query);
    });
  }
  if (activeIndex >= filtered.length) activeIndex = 0;
}

function render(): void {
  if (!listEl) return;

  if (filtered.length === 0) {
    listEl.innerHTML = `<li class="command-palette-empty">No matching commands.</li>`;
    return;
  }

  const html = filtered
    .map((item, idx) => {
      const isActive = idx === activeIndex;
      const cls = isActive ? "command-palette-item command-palette-item-active" : "command-palette-item";
      const hint = item.hint ? `<span class="command-palette-item-hint">${escapeHtml(item.hint)}</span>` : "";
      return `
        <li class="${cls}"
            role="option"
            data-palette-index="${idx}"
            aria-selected="${isActive}">
          <span class="command-palette-item-label">${escapeHtml(labelFor(item))}</span>
          ${hint}
        </li>
      `;
    })
    .join("");

  listEl.innerHTML = html;

  listEl.querySelectorAll<HTMLLIElement>(".command-palette-item").forEach((li) => {
    li.addEventListener("mouseenter", () => {
      const idx = Number(li.dataset.paletteIndex ?? "0");
      activeIndex = idx;
      updateActiveDom();
    });
    li.addEventListener("click", () => {
      const idx = Number(li.dataset.paletteIndex ?? "0");
      activeIndex = idx;
      runActive();
    });
  });
}

function updateActiveDom(): void {
  if (!listEl) return;
  listEl.querySelectorAll<HTMLLIElement>(".command-palette-item").forEach((li, idx) => {
    const isActive = idx === activeIndex;
    li.classList.toggle("command-palette-item-active", isActive);
    li.setAttribute("aria-selected", String(isActive));
  });
}

function moveActive(delta: number): void {
  if (filtered.length === 0) return;
  activeIndex = (activeIndex + delta + filtered.length) % filtered.length;
  updateActiveDom();
  listEl?.querySelector<HTMLLIElement>(`.command-palette-item[data-palette-index="${activeIndex}"]`)?.scrollIntoView({
    block: "nearest",
  });
}

function runActive(): void {
  const action = filtered[activeIndex];
  if (!action) return;
  close();
  action.handler();
}

function escapeHtml(raw: string): string {
  const div = document.createElement("div");
  div.textContent = raw;
  return div.innerHTML;
}

function shouldIntercept(evt: KeyboardEvent): boolean {
  const target = evt.target as HTMLElement | null;
  if (!target) return true;
  const tag = target.tagName.toLowerCase();
  const isTextField = tag === "input" || tag === "textarea" || target.isContentEditable;
  if ((evt.metaKey || evt.ctrlKey) && evt.key.toLowerCase() === "k") {
    return true;
  }

  if (evt.key === "/" && !isTextField) {
    return true;
  }

  return false;
}

export function initCommandPalette(): void {
  document.addEventListener("keydown", (evt) => {
    if (!shouldIntercept(evt)) return;

    if ((evt.metaKey || evt.ctrlKey) && evt.key.toLowerCase() === "k") {
      evt.preventDefault();
      if (overlay && !overlay.hidden) {
        close();
      } else {
        open();
      }

      return;
    }

    if (evt.key === "/" && (overlay === null || overlay.hidden)) {
      evt.preventDefault();
      open();
    }
  });
}
