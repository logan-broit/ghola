// Chapterhouse Admin Portal - UI Components

// Toast notifications
const toast = {
    container: null,

    init() {
        if (this.container) return;
        this.container = document.createElement('div');
        this.container.className = 'toast-container';
        document.body.appendChild(this.container);
    },

    show(message, type = 'success', duration = 4000) {
        this.init();
        const el = document.createElement('div');
        el.className = `toast ${type}`;
        el.innerHTML = `
            <svg class="toast-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                ${type === 'success'
                    ? '<path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/>'
                    : '<circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/>'}
            </svg>
            <span class="toast-message">${message}</span>
            <button class="toast-close" onclick="this.parentElement.remove()">
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                    <line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>
                </svg>
            </button>
        `;
        this.container.appendChild(el);
        setTimeout(() => {
            el.style.opacity = '0';
            el.style.transform = 'translateX(100%)';
            setTimeout(() => el.remove(), 300);
        }, duration);
    },

    success(message) { this.show(message, 'success'); },
    error(message) { this.show(message, 'error', 6000); }
};

// Modal management
const modal = {
    overlay: null,
    current: null,

    init() {
        if (this.overlay) return;
        this.overlay = document.createElement('div');
        this.overlay.className = 'modal-overlay';
        this.overlay.addEventListener('click', (e) => {
            if (e.target === this.overlay) this.close();
        });
        document.body.appendChild(this.overlay);
        document.addEventListener('keydown', (e) => {
            if (e.key === 'Escape' && this.current) this.close();
        });
    },

    open(options) {
        this.init();
        const { title, content, footer, size = 'default' } = options;
        const el = document.createElement('div');
        el.className = `modal ${size === 'large' ? 'modal-lg' : ''}`;
        el.innerHTML = `
            <div class="modal-header">
                <h3 class="modal-title">${title}</h3>
                <button class="modal-close" onclick="modal.close()">
                    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                        <line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>
                    </svg>
                </button>
            </div>
            <div class="modal-body"></div>
            ${footer !== undefined ? '<div class="modal-footer"></div>' : ''}
        `;
        const bodyEl = el.querySelector('.modal-body');
        if (typeof content === 'string') bodyEl.innerHTML = content;
        else bodyEl.appendChild(content);

        if (footer !== undefined) {
            const footerEl = el.querySelector('.modal-footer');
            if (typeof footer === 'string') footerEl.innerHTML = footer;
            else footerEl.appendChild(footer);
        }

        this.overlay.innerHTML = '';
        this.overlay.appendChild(el);
        this.current = el;
        requestAnimationFrame(() => this.overlay.classList.add('active'));
        return el;
    },

    close() {
        if (!this.overlay) return;
        this.overlay.classList.remove('active');
        this.current = null;
    },

    confirm(message, title = 'Confirm') {
        return new Promise((resolve) => {
            const footerEl = document.createElement('div');
            footerEl.innerHTML = `
                <button class="btn btn-secondary" id="modal-cancel">Cancel</button>
                <button class="btn btn-danger" id="modal-confirm">Confirm</button>
            `;
            this.open({ title, content: `<p>${message}</p>`, footer: footerEl });
            footerEl.querySelector('#modal-cancel').onclick = () => { this.close(); resolve(false); };
            footerEl.querySelector('#modal-confirm').onclick = () => { this.close(); resolve(true); };
        });
    }
};

// Loading state
const loading = {
    show(element) {
        if (!element) return;
        element.classList.add('loading');
        const overlay = document.createElement('div');
        overlay.className = 'loading-overlay';
        overlay.innerHTML = '<div class="loading-spinner"></div>';
        element.style.position = 'relative';
        element.appendChild(overlay);
    },

    hide(element) {
        if (!element) return;
        element.classList.remove('loading');
        const overlay = element.querySelector('.loading-overlay');
        if (overlay) overlay.remove();
    }
};

// Sidebar navigation
function initSidebar() {
    const currentPage = window.location.pathname.split('/').pop() || 'admin.html';
    document.querySelectorAll('.nav-item').forEach(item => {
        const href = item.getAttribute('href');
        if (href === currentPage) item.classList.add('active');
        else item.classList.remove('active');
    });

    // Role-based visibility
    const user = chapterhouse.auth.getCurrentUser();
    if (user) {
        document.querySelectorAll('[data-admin-only]').forEach(el => {
            el.style.display = user.is_admin ? '' : 'none';
        });
        document.querySelectorAll('[data-user-only]').forEach(el => {
            el.style.display = user.is_admin ? 'none' : '';
        });
    }
}

// User display in sidebar
async function initUserDisplay() {
    try {
        const user = await chapterhouse.auth.me();
        updateUserDisplay(user);
        initSidebar();
    } catch (err) {
        console.error('Auth check failed:', err);
        window.location.href = 'login.html';
    }
}

function updateUserDisplay(user) {
    const avatarEl = document.getElementById('user-avatar');
    const nameEl = document.getElementById('user-name');
    const roleEl = document.getElementById('user-role');
    if (avatarEl) avatarEl.textContent = chapterhouse.getInitials(user.display_name || user.username);
    if (nameEl) nameEl.textContent = user.display_name || user.username;
    if (roleEl) roleEl.textContent = user.is_admin ? 'Administrator' : 'User';
}

// Logout
async function logout() {
    try { await chapterhouse.auth.logout(); } catch (err) { console.error('Logout error:', err); }
    window.location.href = 'login.html';
}

// Copy to clipboard
async function copyToClipboard(text, successMessage = 'Copied!') {
    try {
        await navigator.clipboard.writeText(text);
        toast.success(successMessage);
    } catch (err) {
        // Fallback for non-secure contexts (self-signed certs, HTTP)
        try {
            const ta = document.createElement('textarea');
            ta.value = text;
            ta.style.position = 'fixed';
            ta.style.opacity = '0';
            document.body.appendChild(ta);
            ta.select();
            document.execCommand('copy');
            document.body.removeChild(ta);
            toast.success(successMessage);
        } catch (e) {
            toast.error('Failed to copy');
        }
    }
}

// Table rendering
function renderTable(containerId, columns, data, options = {}) {
    const container = document.getElementById(containerId);
    if (!container) return;

    if (!data || data.length === 0) {
        container.innerHTML = `
            <div class="empty-state">
                <svg class="empty-state-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
                    <path d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"/>
                </svg>
                <h3 class="empty-state-title">${options.emptyTitle || 'No data found'}</h3>
                <p class="empty-state-text">${options.emptyText || 'There are no items to display.'}</p>
            </div>
        `;
        return;
    }

    const headerHtml = columns.map(col => `<th>${col.label}</th>`).join('');
    const rowsHtml = data.map(item => {
        const cells = columns.map(col => {
            const value = col.render ? col.render(item) : item[col.key];
            return `<td>${value ?? '-'}</td>`;
        }).join('');
        return `<tr>${cells}</tr>`;
    }).join('');

    container.innerHTML = `
        <div class="table-container">
            <table class="table">
                <thead><tr>${headerHtml}</tr></thead>
                <tbody>${rowsHtml}</tbody>
            </table>
        </div>
    `;
}

// Pagination
function renderPagination(containerId, total, offset, limit, onPageChange) {
    const container = document.getElementById(containerId);
    if (!container) return;

    const start = offset + 1;
    const end = Math.min(offset + limit, total);
    const hasPrev = offset > 0;
    const hasNext = offset + limit < total;

    container.innerHTML = `
        <div class="pagination">
            <span class="pagination-info">Showing ${start} to ${end} of ${total}</span>
            <div class="pagination-controls">
                <button class="btn btn-sm btn-secondary" id="page-prev" ${hasPrev ? '' : 'disabled'}>Previous</button>
                <button class="btn btn-sm btn-secondary" id="page-next" ${hasNext ? '' : 'disabled'}>Next</button>
            </div>
        </div>
    `;

    container.querySelector('#page-prev')?.addEventListener('click', () => {
        if (hasPrev) onPageChange(Math.max(0, offset - limit));
    });
    container.querySelector('#page-next')?.addEventListener('click', () => {
        if (hasNext) onPageChange(offset + limit);
    });
}

// Distribution bar chart
function renderDistribution(containerId, data, labelKey, valueKey) {
    const container = document.getElementById(containerId);
    if (!container) return;
    if (!data || data.length === 0) {
        container.innerHTML = '<p class="text-muted">No data available</p>';
        return;
    }

    const max = Math.max(...data.map(d => d[valueKey]));
    const colors = ['blue', 'purple', 'pink', 'green', 'yellow', 'orange', 'cyan'];

    container.innerHTML = data.map((item, i) => `
        <div class="distribution-bar">
            <div class="distribution-label">
                <span class="distribution-label-text">${item[labelKey]}</span>
                <span class="distribution-label-value">${item[valueKey]}</span>
            </div>
            <div class="distribution-track">
                <div class="distribution-fill ${colors[i % colors.length]}" style="width: ${(item[valueKey] / max) * 100}%"></div>
            </div>
        </div>
    `).join('');
}

// Tag cloud
function renderTagCloud(containerId, tags) {
    const container = document.getElementById(containerId);
    if (!container) return;
    if (!tags || tags.length === 0) {
        container.innerHTML = '<p class="text-muted">No tags found</p>';
        return;
    }

    container.innerHTML = `<div class="tag-cloud">${tags.map(t =>
        `<span class="tag-item">${t.tag} <span class="tag-count">${t.count}</span></span>`
    ).join('')}</div>`;
}

// Badge helpers
function statusBadge(status, label = null) {
    const cls = {
        'active': 'active', 'running': 'active', 'success': 'success',
        'pending': 'pending', 'stopped': 'warning',
        'inactive': 'inactive', 'disabled': 'inactive', 'deactivated': 'inactive',
        'revoked': 'revoked', 'expired': 'revoked',
        'error': 'error', 'failed': 'error',
    }[status?.toLowerCase()] || 'info';
    return `<span class="status-badge ${cls}"><span class="status-dot"></span>${label || status}</span>`;
}

function roleBadge(isAdmin) {
    return isAdmin
        ? '<span class="role-badge admin">Admin</span>'
        : '<span class="role-badge user">User</span>';
}

function actionBadge(action) {
    const cls = action.replace(/\./g, '-');
    return `<span class="action-badge ${cls}">${action}</span>`;
}

function resourceBadge(type) {
    return `<span class="resource-badge">${type}</span>`;
}

// Form helpers
function getFormData(formEl) {
    const data = {};
    for (const [key, value] of new FormData(formEl).entries()) {
        data[key] = value;
    }
    // Handle checkboxes
    formEl.querySelectorAll('input[type="checkbox"]').forEach(cb => {
        data[cb.name] = cb.checked;
    });
    return data;
}

function setFormErrors(formEl, errors) {
    formEl.querySelectorAll('.form-error').forEach(el => el.remove());
    formEl.querySelectorAll('.form-input.error').forEach(el => el.classList.remove('error'));
    for (const [field, message] of Object.entries(errors)) {
        const input = formEl.querySelector(`[name="${field}"]`);
        if (input) {
            input.classList.add('error');
            const errorEl = document.createElement('div');
            errorEl.className = 'form-error';
            errorEl.textContent = message;
            input.parentNode.appendChild(errorEl);
        }
    }
}

// Initialize on load
document.addEventListener('DOMContentLoaded', () => {
    if (document.getElementById('user-avatar')) {
        initUserDisplay();
    }
});

// Export globals
window.toast = toast;
window.modal = modal;
window.loading = loading;
window.logout = logout;
window.copyToClipboard = copyToClipboard;
window.renderTable = renderTable;
window.renderPagination = renderPagination;
window.renderDistribution = renderDistribution;
window.renderTagCloud = renderTagCloud;
window.statusBadge = statusBadge;
window.roleBadge = roleBadge;
window.actionBadge = actionBadge;
window.resourceBadge = resourceBadge;
window.getFormData = getFormData;
window.setFormErrors = setFormErrors;
