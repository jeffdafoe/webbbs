<?php

namespace App\Door\ZChat\Entity\Enum;

enum UserStatus: string
{
    case ENTERING = 'entering';
    case NORMAL = 'normal';
    case AWAY = 'away';
    case BUSY = 'busy';
    case IN_PRIVATE_CHAT = 'in_private_chat';
    case IN_GAME = 'in_game';
    case SILENCED = 'silenced';
}
