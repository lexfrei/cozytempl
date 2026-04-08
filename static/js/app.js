// cozytempl — Alpine.js application

const API_BASE = '/api';

// API client
async function api(path, options = {}) {
  const resp = await fetch(API_BASE + path, {
    headers: { 'Content-Type': 'application/json', ...options.headers },
    ...options,
  });

  if (resp.status === 401) {
    window.location.href = '/auth/login';
    return null;
  }

  if (!resp.ok) {
    const body = await resp.json().catch(() => ({ error: resp.statusText }));
    throw new Error(body.error || resp.statusText);
  }

  if (resp.status === 204) return null;
  return resp.json();
}

// Date formatting
function formatDate(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleDateString('en-US', {
    month: 'short', day: 'numeric', year: 'numeric',
    hour: '2-digit', minute: '2-digit',
  });
}

// Clipboard
async function copyToClipboard(text) {
  await navigator.clipboard.writeText(text);
}

// Main app store
function app() {
  return {
    currentPage: 'dashboard',
    currentTenant: '',      // namespace, e.g. "tenant-root"
    currentTenantName: '',  // display name, e.g. "root"
    currentApp: '',
    toasts: [],
    _toastId: 0,

    init() {
      this.handleRoute();
      window.addEventListener('popstate', () => this.handleRoute());
    },

    handleRoute() {
      const path = window.location.pathname;
      if (path.startsWith('/tenants/') && path.includes('/apps/')) {
        const parts = path.split('/');
        this.currentTenant = parts[2];
        this.currentTenantName = parts[2];
        this.currentApp = parts[4];
        this.currentPage = 'appDetail';
      } else if (path.startsWith('/tenants/')) {
        this.currentTenant = path.split('/')[2];
        this.currentTenantName = this.currentTenant;
        this.currentPage = 'tenant';
      } else if (path === '/marketplace') {
        this.currentPage = 'marketplace';
      } else {
        this.currentPage = 'dashboard';
      }
    },

    navigate(page, tenantNS, tenantName, appName) {
      this.currentPage = page;
      if (tenantNS) this.currentTenant = tenantNS;
      if (tenantName) this.currentTenantName = tenantName;
      if (appName) this.currentApp = appName;

      let url = '/';
      if (page === 'marketplace') {
        url = '/marketplace';
      } else if (page === 'tenant' && this.currentTenant) {
        url = '/tenants/' + this.currentTenant;
      } else if (page === 'appDetail' && this.currentTenant && this.currentApp) {
        url = '/tenants/' + this.currentTenant + '/apps/' + this.currentApp;
      }
      history.pushState({}, '', url);
    },

    addToast(type, message) {
      const id = ++this._toastId;
      this.toasts.push({ id, type, message });
    },

    removeToast(id) {
      this.toasts = this.toasts.filter(t => t.id !== id);
    },

    connectSSE() {
      if (!this.currentTenant) return;

      const source = new EventSource(API_BASE + '/events?tenant=' + this.currentTenant);
      source.onmessage = (event) => {
        const data = JSON.parse(event.data);
        const badge = document.getElementById('status-' + data.name);
        if (badge) {
          badge.className = 'badge badge-' + data.data.toLowerCase();
          badge.textContent = data.data;
        }
      };
      source.onerror = () => {
        source.close();
        setTimeout(() => this.connectSSE(), 5000);
      };
    },
  };
}

// Tenant tree component
function tenantTree() {
  return {
    tenants: [],
    tenantFilter: '',
    loading: true,

    async init() {
      try {
        this.tenants = await api('/tenants') || [];
      } catch (err) {
        console.error('Failed to load tenants:', err);
        this.tenants = [];
      }
      this.loading = false;
    },

    get filteredRootTenants() {
      const roots = this.tenants.filter(t => !t.parent);
      if (!this.tenantFilter) return roots;
      const q = this.tenantFilter.toLowerCase();
      return roots.filter(t =>
        t.name.toLowerCase().includes(q) ||
        (t.displayName && t.displayName.toLowerCase().includes(q))
      );
    },

    getChildren(parentNS) {
      return this.tenants.filter(t => t.parent === parentNS);
    },

    selectTenant(tenant) {
      const appStore = Alpine.closestDataStack(this.$el).find(s => s.navigate);
      if (appStore) {
        appStore.navigate('tenant', tenant.namespace, tenant.displayName);
      }
    },
  };
}

// Marketplace component
function marketplace() {
  return {
    schemas: [],
    search: '',
    categoryFilter: '',
    tagFilter: '',
    selectedApp: null,
    loading: true,

    async init() {
      try {
        this.schemas = await api('/schemas') || [];
      } catch (err) {
        console.error('Failed to load schemas:', err);
      }
      this.loading = false;
    },

    get categories() {
      return [...new Set(this.schemas.map(s => s.category || 'Other'))].sort();
    },

    get allTags() {
      const tags = new Set();
      for (const s of this.schemas) {
        for (const t of (s.tags || [])) tags.add(t);
      }
      return [...tags].sort();
    },

    get filteredSchemas() {
      let list = this.schemas;

      if (this.categoryFilter) {
        list = list.filter(s => (s.category || 'Other') === this.categoryFilter);
      }

      if (this.tagFilter) {
        list = list.filter(s => (s.tags || []).includes(this.tagFilter));
      }

      if (this.search) {
        const q = this.search.toLowerCase();
        list = list.filter(s =>
          s.kind.toLowerCase().includes(q) ||
          (s.displayName || '').toLowerCase().includes(q) ||
          (s.description || '').toLowerCase().includes(q) ||
          (s.tags || []).some(t => t.toLowerCase().includes(q))
        );
      }

      return list;
    },

    get filteredCategories() {
      const cats = new Set(this.filteredSchemas.map(s => s.category || 'Other'));
      return [...cats].sort();
    },

    schemasInCategory(cat) {
      return this.schemas.filter(s => (s.category || 'Other') === cat).length;
    },

    appsInCategory(cat) {
      return this.filteredSchemas
        .filter(s => (s.category || 'Other') === cat)
        .sort((a, b) => (a.displayName || a.kind).localeCompare(b.displayName || b.kind));
    },

    schemaFieldCount(app) {
      if (!app?.jsonSchema?.properties) return 0;
      return Object.keys(app.jsonSchema.properties).length;
    },

    schemaProperties(app) {
      if (!app?.jsonSchema?.properties) return [];
      return Object.entries(app.jsonSchema.properties);
    },

    showDetail(app) {
      this.selectedApp = app;
    },

    deployApp(app) {
      this.selectedApp = null;
      const appStore = Alpine.closestDataStack(this.$el).find(s => s.navigate);
      if (appStore) {
        if (appStore.currentTenant) {
          appStore.navigate('tenant', appStore.currentTenant, appStore.currentTenantName);
        } else {
          appStore.navigate('dashboard');
        }
      }
    },
  };
}

// Dashboard component
function dashboard() {
  return {
    stats: { tenants: '-', apps: '-', ready: '-', failed: '-' },

    async init() {
      try {
        const tenants = await api('/tenants') || [];
        this.stats.tenants = tenants.length;

        // Load apps from all tenants using namespace
        const appPromises = tenants.map(t =>
          api('/tenants/' + t.namespace + '/apps').catch(() => [])
        );
        const allApps = (await Promise.all(appPromises)).flat();

        this.stats.apps = allApps.length;
        this.stats.ready = allApps.filter(a => a.status === 'Ready').length;
        this.stats.failed = allApps.filter(a => a.status === 'Failed').length;
      } catch (err) {
        console.error('Failed to load dashboard stats:', err);
      }
    },
  };
}

// App list component
function appList() {
  return {
    apps: [],
    appFilter: '',
    kindFilter: '',
    sortBy: 'name',
    showCreateModal: false,
    loading: true,

    async init() {
      await this.loadApps();
    },

    async loadApps() {
      const appStore = Alpine.closestDataStack(this.$el).find(s => s.currentTenant);
      const tenant = appStore?.currentTenant;
      if (!tenant) return;

      try {
        this.apps = await api('/tenants/' + tenant + '/apps') || [];
      } catch (err) {
        console.error('Failed to load apps:', err);
        this.apps = [];
      }
      this.loading = false;
    },

    get availableKinds() {
      return [...new Set(this.apps.map(a => a.kind))].sort();
    },

    get filteredApps() {
      let list = this.apps;

      if (this.kindFilter) {
        list = list.filter(a => a.kind === this.kindFilter);
      }

      if (this.appFilter) {
        const q = this.appFilter.toLowerCase();
        list = list.filter(a => a.name.toLowerCase().includes(q));
      }

      list = [...list].sort((a, b) => {
        const key = this.sortBy;
        const va = a[key] || '';
        const vb = b[key] || '';
        return String(va).localeCompare(String(vb));
      });

      return list;
    },

    selectApp(name) {
      const appStore = Alpine.closestDataStack(this.$el).find(s => s.navigate);
      if (appStore) {
        appStore.navigate('appDetail', appStore.currentTenant, appStore.currentTenantName, name);
      }
    },

    async confirmDelete(application) {
      if (!confirm('Delete ' + application.kind + ' "' + application.name + '"? This cannot be undone.')) {
        return;
      }

      const appStore = Alpine.closestDataStack(this.$el).find(s => s.currentTenant);
      const tenant = appStore?.currentTenant;

      try {
        await api('/tenants/' + tenant + '/apps/' + application.name, { method: 'DELETE' });
        this.apps = this.apps.filter(a => a.name !== application.name);
        const toaster = Alpine.closestDataStack(this.$el).find(s => s.addToast);
        toaster?.addToast('success', application.name + ' deleted');
      } catch (err) {
        const toaster = Alpine.closestDataStack(this.$el).find(s => s.addToast);
        toaster?.addToast('error', 'Delete failed: ' + err.message);
      }
    },

    formatDate,
  };
}

// App form component (create modal)
function appForm() {
  return {
    schemas: [],
    selectedKind: '',
    appName: '',
    formValues: {},
    currentSchema: null,

    async init() {
      try {
        this.schemas = await api('/schemas') || [];
      } catch (err) {
        console.error('Failed to load schemas:', err);
      }
    },

    async loadSchema() {
      if (!this.selectedKind) {
        this.currentSchema = null;
        this.$refs.schemaFields.innerHTML = '';
        return;
      }

      try {
        this.currentSchema = await api('/schemas/' + this.selectedKind);
        this.formValues = {};
        this.renderSchemaForm();
      } catch (err) {
        console.error('Failed to load schema:', err);
      }
    },

    renderSchemaForm() {
      const container = this.$refs.schemaFields;
      container.innerHTML = '';
      if (!this.currentSchema?.jsonSchema?.properties) return;

      const props = this.currentSchema.jsonSchema.properties;
      const required = this.currentSchema.jsonSchema.required || [];

      for (const [key, prop] of Object.entries(props)) {
        const group = document.createElement('div');
        group.className = 'form-group';

        const label = document.createElement('label');
        label.className = 'form-label';
        label.textContent = prop.title || key;
        if (required.includes(key)) label.textContent += ' *';
        group.appendChild(label);

        let input;
        if (prop.enum) {
          input = document.createElement('select');
          input.className = 'form-select';
          for (const val of prop.enum) {
            const opt = document.createElement('option');
            opt.value = val;
            opt.textContent = val;
            input.appendChild(opt);
          }
        } else if (prop.type === 'boolean') {
          input = document.createElement('select');
          input.className = 'form-select';
          for (const val of ['true', 'false']) {
            const opt = document.createElement('option');
            opt.value = val;
            opt.textContent = val;
            input.appendChild(opt);
          }
        } else if (prop.type === 'integer' || prop.type === 'number') {
          input = document.createElement('input');
          input.className = 'form-input';
          input.type = 'number';
          if (prop.minimum !== undefined) input.min = prop.minimum;
          if (prop.maximum !== undefined) input.max = prop.maximum;
        } else {
          input = document.createElement('input');
          input.className = 'form-input';
          input.type = 'text';
          if (prop.pattern) input.pattern = prop.pattern;
        }

        input.dataset.key = key;
        const defaultVal = this.formValues[key] ?? prop.default ?? '';
        input.value = String(defaultVal);
        input.addEventListener('input', (e) => {
          let val = e.target.value;
          if (prop.type === 'integer') val = parseInt(val, 10);
          else if (prop.type === 'number') val = parseFloat(val);
          else if (prop.type === 'boolean') val = val === 'true';
          this.formValues[key] = val;
        });
        group.appendChild(input);

        if (prop.description) {
          const hint = document.createElement('div');
          hint.className = 'form-hint';
          hint.textContent = prop.description;
          group.appendChild(hint);
        }

        container.appendChild(group);
      }
    },

    async submitCreate() {
      const appStore = Alpine.closestDataStack(this.$el).find(s => s.currentTenant);
      const tenant = appStore?.currentTenant;

      try {
        await api('/tenants/' + tenant + '/apps', {
          method: 'POST',
          body: JSON.stringify({
            name: this.appName,
            kind: this.selectedKind,
            spec: this.formValues,
          }),
        });
        const toaster = Alpine.closestDataStack(this.$el).find(s => s.addToast);
        toaster?.addToast('success', this.appName + ' created');

        this.appName = '';
        this.selectedKind = '';
        this.formValues = {};
        this.$refs.schemaFields.innerHTML = '';

        const listStore = Alpine.closestDataStack(this.$el).find(s => s.showCreateModal !== undefined);
        if (listStore) {
          listStore.showCreateModal = false;
          listStore.loadApps();
        }
      } catch (err) {
        const toaster = Alpine.closestDataStack(this.$el).find(s => s.addToast);
        toaster?.addToast('error', 'Create failed: ' + err.message);
      }
    },
  };
}

// App detail component
function appDetail() {
  return {
    detail: {},
    tab: 'overview',

    async init() {
      await this.loadDetail();
    },

    async loadDetail() {
      const appStore = Alpine.closestDataStack(this.$el).find(s => s.currentTenant);
      const tenant = appStore?.currentTenant;
      const appName = appStore?.currentApp;
      if (!tenant || !appName) return;

      try {
        this.detail = await api('/tenants/' + tenant + '/apps/' + appName) || {};
      } catch (err) {
        console.error('Failed to load app detail:', err);
      }
    },

    formatDate,

    async copyToClipboard(text) {
      await navigator.clipboard.writeText(text);
      const toaster = Alpine.closestDataStack(this.$el).find(s => s.addToast);
      toaster?.addToast('success', 'Copied to clipboard');
    },
  };
}
