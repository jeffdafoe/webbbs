import { Terminal } from '@xterm/xterm';
import { TerminalIO, MenuItem } from './terminal-io';
import { WidgetPosition, WidgetItem } from './template-renderer';

export type RouteHandler = () => Promise<string | null>;

export class ScreenRunner {
    private io: TerminalIO;
    private routes: Map<string, RouteHandler>;

    constructor(
        private terminal: Terminal,
        routes: Map<string, RouteHandler>
    ) {
        this.io = new TerminalIO(terminal);
        this.routes = routes;
    }

    async run(widgets: WidgetPosition[]): Promise<string | null> {
        const menuWidget = widgets.find(w => w.type === 'menu');
        if (!menuWidget) {
            return null;
        }

        const items: MenuItem[] = [];
        for (let i = 0; i < menuWidget.items.length; i++) {
            const widgetItem = menuWidget.items[i];
            items.push({
                label: widgetItem.label,
                value: widgetItem.route,
                hotkey: widgetItem.label[0],
            });
        }

        if (items.length === 0) {
            return null;
        }

        const position = {
            row: menuWidget.row,
            col: menuWidget.col,
            width: menuWidget.width,
        };

        const choice = await this.io.lightbar(items, { position });

        if (choice === null) {
            return null;
        }

        await this.io.sleep(150);

        const handler = this.routes.get(choice);
        if (handler) {
            return handler();
        }

        return choice;
    }
}
